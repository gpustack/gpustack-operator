# Spec: Accelerator Slicing Metadata Realignment — Per-Card Capability, Group SlicedDetail, InstanceType Observed Detail

Status: Shipped
Type: Feature

> **Supersedes prior design.** This spec supersedes the parts of earlier shipped specs it changes — the
> supersession is declared here, in the active spec's header, and the archived specs are left untouched:
> - `2026-07-16-accelerator-slicing-capability-and-pool-feedback` — its group-level `AcceleratorsFeature` /
>   `AcceleratorSliced` model and detector-`LogicalSliced` wiring are replaced by per-card
>   `AcceleratorStatus.LogicalSliced` / `PhysicalSliced`, aggregated into the group's `AcceleratorSlicedDetail`;
>   the `AcceleratorsFeature` family (incl. `MaxSlices()`) and the transitional old-format dual-read fallback
>   are removed. The per-vendor soft-slice counts and detection gates that spec lists remain current.
> - `2026-06-25-accelerator-soft-slicing-runtime-isolation` — the `--slicing-policy` knob it introduced is
>   removed (dead).

## Summary
Restructure how slicing capability flows through the scheduling chain, as the first of two specs on the road to
NVIDIA MIG allocation. Each accelerator card gains a structured per-card capability record
(`LogicalSliced` soft-slicing count + `PhysicalSliced` real MIG profile inventory, enumerated from NVML instead
of today's `MaxSize: 7 // TODO` placeholder), the device group gains an aggregated `AcceleratorSlicedDetail`
that replaces the group-level `AcceleratorsFeature`, and every downstream consumer re-plumbs: the
NodeCapacityReconciler reads the aggregate (and starts watching Devices), the device-plugin token pool sizes
per card, the `acceleratorFeature` ResourceFlavor note and its webhook transcription path are removed,
InstanceType separates desired spec from observed hardware detail (`Status.Detail`, backfilled by the
reconciler), and the WorkerGateway aggregation carries the detail level-by-level. The dead `--slicing-policy`
flag and the write-only `MemoryPercentageStep` field are deleted. This spec is pure metadata/view realignment:
no new schedulable resource and no allocation-path change land here — the `.sliced.mig-<profile>` resource keys,
webhook validation, AdmissionCheck profile filtering, and GI/CI create/destroy are the follow-up
"dynamic MIG implementation" spec.

## Motivation

Today the slicing capability is recorded once per device group (`DevicesGroup.AcceleratorsFeature`), with three
structural lies baked in:

1. **The NVIDIA MIG capability is a placeholder.** The detector seeds
   `PhysicalSliced{MaxSize: 7 /* TODO */, MemoryPercentageStep: 25 /* TODO */}` when a card has MIG mode
   enabled (or pending), and only inside the "first card creates the group" branch
   (`pkg/devicemanager/detector/nvidia/device.go:188-197`) — a card that enables MIG after its group exists
   never surfaces the capability at all. No real profile inventory (names, memory, slice geometry, instance
   counts) exists anywhere in the system.
2. **Group-level capability cannot express per-card mutual exclusion.** A MIG-enabled card must not serve
   soft slices (the vendor interception library cannot cap a hardware-partitioned card), yet the group-level
   `MaxSlices()` — max of logical 128 and physical 7 — advertises 128 soft-slice tokens for *every* card in the
   group, MIG-enabled ones included.
3. **The InstanceType copies observed hardware into its desired spec** via a JSON note on the ResourceFlavor
   (`acceleratorFeature`), snapshotted once at admission by the mutating webhook. Capability now changes at
   DeviceManager restart (MIG is enabled manually, below), so an admission-time snapshot transcribed through an
   annotation is both stale-by-design and the wrong side of the desired/observed split.

Separately, the `--slicing-policy` allocator flag (best-effort/qos) is plumbed into `AllocatorOptions` but read
by **zero** of the nine vendor subpackages, and `MemoryPercentageStep` is written by all six detectors (always
`1`, plus the MIG placeholder `25`) and read by nothing.

### Goals
- **Truthful per-card capability.** Every card records its own `LogicalSliced` (soft-slice count + overcommit
  flag) and `PhysicalSliced` (real MIG profile inventory). A currently MIG-enabled card has
  `LogicalSliced.Count == 0` and non-empty `Profiles`; every other card has the inverse. "A MIG card does not
  participate in soft slicing" becomes a property of the data, not a runtime policy check.
- **Real NVML profile enumeration on NVIDIA.** Replace the placeholder seed with per-card GI-profile
  enumeration, filtered to C==G profiles and excluding `+me`/`+gfx` variants (the same rule NVIDIA's
  k8s-device-plugin `mixed` strategy applies), fixing the first-card-only-seed defect as a side effect.
- **One aggregated group view.** `DevicesGroup.AcceleratorSlicedDetail` (logical count summed across cards,
  physical profile counts summed by name) replaces `AcceleratorsFeature`; the `MaxSlices()` /
  `SlicedCoresOvercommit()` helpers and the `MemoryPercentageStep` field are deleted, and all four consumers
  re-plumb (NodeCapacityReconciler, device-plugin `ListAndWatch` token pool, flavor catalog projection,
  `IsSliceable()`).
- **Desired/observed separation on InstanceType.** Spec keeps admin inputs (plus a new `Description` field);
  Manufacturer/Product/Family and the inlined CPU/Accelerator descriptors move into a new
  `Status.Detail` (`InstanceTypeDetail`), backfilled by the InstanceTypeReconciler (which already watches
  Devices) instead of by the mutating webhook from a flavor note. `InstanceTypeSpec` stays fully comparable.
- **WorkerGateway aggregation follows**, exposing `InstanceTypeDetail` and `AcceleratorSlicedDetail` at
  candidate/tier/top level so multi-cluster consumers can read hardware detail from the observed side.
- **Dead plumbing removed**: `--slicing-policy` (with a deployment-compatibility path) and
  `MemoryPercentageStep`.
- **Manual MIG lifecycle documented**: enable/disable per card or per node with `nvidia-smi`, restart the
  DeviceManager to re-detect, recover after node reboot. Zero automation.

**Success criteria (measurable):**
1. On a node with no MIG-enabled card, the four advertised `.sliced.*` node capacity values
   (`.sliced.units`, `.sliced.cores-percentage`, `.sliced.memory-percentage`, `.sliced.memory-mib`) are
   **numerically identical** before and after this spec, and the device-plugin advertises the same per-card
   token counts. Cluster-observable behavior is unchanged.
2. On a MIG-enabled A100-40GB card, the Devices ledger shows `LogicalSliced.Count == 0` and
   `PhysicalSliced.Profiles` matching the supported-profile table below (names, MemoryMib, slice geometry,
   per-card counts), with `+me`/`+gfx` variants absent.
3. `kubectl get resourceflavors -o yaml` shows no `acceleratorFeature` note; a freshly derived InstanceType
   reaches `Status.Detail` populated (manufacturer/product/family/CPU/accelerator/`SlicedDetail`) within one
   reconcile of its Devices source.
4. `grep -r "slicing-policy\|SlicingPolicy\|AcceleratorsFeature\|MemoryPercentageStep\|MaxSlices\|SlicedCoresOvercommit"`
   over non-generated, non-test Go code returns nothing (modulo an optional one-release deprecation shim for the
   flag).
5. `make generate` is clean (CRDs, deepcopy, protobuf, applyconfigurations regenerate without manual edits),
   `go build ./...`, `go test ./...`, and `make lint` pass.

### Non-Goals
- **Everything in the follow-up "dynamic MIG implementation" spec**: the `.sliced.mig-<profile>` extended
  resource keys and their capacity reporting, Pod webhook validation/defaulting for `mig-*` requests (including
  initContainers coverage), AdmissionCheck per-card profile filtering and the static-unsatisfiable-Reject /
  fragmentation-Retry split, DeviceManager GI/CI incremental create/destroy with placement awareness, the
  per-card Allocated/Free profile ledger in `Devices.Status`, and real-hardware E2E.
- **Any memory→profile translation.** The resource-key semantics are split for good:
  `.sliced.cores-percentage` / `.sliced.memory-percentage` / `.sliced.memory-mib` belong exclusively to
  logical (soft) slicing; `.sliced.mig-<profile>` (spec 2) belongs exclusively to physical (MIG) slicing. A
  physical request names its profile explicitly — the same user contract as NVIDIA's `mixed` strategy and the
  DRA driver's CEL attribute match. "Users must know profile names" is an accepted cost.
- **Any MIG mode automation.** No HAMi-style nodeconfig/label-triggered mode switching, no reconciler that
  flips MIG mode. Mode changes are administrator-driven node operations (documented here), and capability
  changes enter the system only through DeviceManager re-detection.
- **Geometry-level dynamics** (whole-card re-slicing, defragmentation) — rejected permanently, not deferred:
  re-slicing requires an idle card and is eviction by another name.
- **Changing Kueue quota semantics, the Pod webhook's soft-slicing numeric alignment, or the AdmissionCheck
  logic.** They keep consuming the same four soft keys with the same values.

## Proposal

Restructure the metadata in four layers — per-card, group, node/flavor, instance-type/gateway — so that after
this spec the cluster behaves identically (on non-MIG hardware) but every layer records what is actually true,
and the follow-up spec can build MIG allocation on structured facts instead of placeholders.

### Grounding facts (copied verbatim from the research round; sources are NVIDIA official docs/source)

**Key-semantics split.** The whole ecosystem except HAMi anchors MIG requests on profile *names*: NVIDIA
k8s-device-plugin `mixed` registers `nvidia.com/mig-<profile>` per profile; the NVIDIA DRA driver matches
`device.attributes['gpu.nvidia.com'].profile == '1g.5gb'`; `mig-parted` configs and `nvidia-smi` speak profile
names. Fraction anchoring fails structurally: on A100-40GB, `1g.10gb` and `2g.10gb` have identical memory
(2/8) and differ only in compute, so a memory fraction cannot pick between them; across generations the same
fraction lands on different semantics (H100's `1g.20gb` is 1/7 SM but 1/4 memory — no A100 profile has that
ratio); and a scalar cannot express placement/combination legality, so it cannot defend against stranded
capacity.

**A100-40GB supported profiles (ignoring `+me`/`+gfx` variants).** Memory slice = 1/8 of VRAM, SM slice = 1/7
of SMs:

| Profile | Memory | Compute slices (of 7) | Memory slices (of 8) | Max instances/card |
|---|---|---|---|---|
| 1g.5gb  | 5 GB  | 1 | 1 | 7 |
| 1g.10gb | 10 GB | 1 | 2 | 4 |
| 2g.10gb | 10 GB | 2 | 2 | 3 |
| 3g.20gb | 20 GB | 3 | 4 | 2 |
| 4g.20gb | 20 GB | 4 | 4 | 1 |
| 7g.40gb | 40 GB | 7 | 8 | 1 |

`+me` (dedicated media engines: 1 NVDEC/1 JPEG/1 OFA) and `+gfx` (graphics-capable, Blackwell only) are not
separate fields at the NVML layer — they are dedicated GI profile IDs (e.g.
`GPU_INSTANCE_PROFILE_1_SLICE_REV1` → attribute `"me"`), so filtering them means an allow-list on GI profile
ID/attribute, structurally the same filter k8s-device-plugin `mixed` applies by only registering C==G
profiles.

**Placement slots are hardcoded** (legal start:size in memory-slice units, from NVML placement data):
`1g.5gb` any of 0–6 (size 1); `1g.10gb` starts 0/2/4/6 (size 2); `2g.10gb` starts 0/2/4 only (size 2);
`3g.20gb` starts 0 or 4 (size 4); `4g.20gb` start 0 only (size 4); `7g.40gb` start 0 (size 8). Combination
legality = the occupied slot intervals do not overlap. This is why per-profile *counts* (this spec) are only
static potential; the placement-aware Free ledger is spec 2.

**MIG lifecycle costs.**
- *Mode switch (card-level, heavy):* Ampere (A100/A30) requires a GPU reset after enabling, and the mode
  persists across reboots via InfoROM; Hopper+ needs no reset but the mode is **not** persistent across
  reboots. All daemons holding driver handles (DCGM, nvsm, exporters) must stop first or the switch hangs
  pending; a loaded `nvidia_drm` kernel module makes the reset fail; in passthrough VMs the hypervisor may
  forbid the reset entirely (reboot the VM instead). Requires `CAP_SYS_ADMIN`.
- *Instance management (GI/CI, light):* once mode is on, instance create/destroy is dynamic and online.
  `nvmlGpuInstanceDestroy`/`nvmlComputeInstanceDestroy` return `IN_USE` only when the instance **itself** has
  active processes — workloads on sibling instances of the same card are unaffected. MIG instances never
  persist across reboots.

### User Stories

#### Story 1
As a **cluster administrator**, I want to enable MIG on specific cards with `nvidia-smi` and restart the
DeviceManager, and then see the Devices ledger report each card's real profile inventory (and zero soft-slice
capability on those cards), so that I can trust the cluster's advertised slicing capability instead of
placeholder numbers.

#### Story 2
As a **platform operator consuming the WorkerGateway** (UI/API), I want instance-type hardware detail —
manufacturer, product, CPU, accelerator, and slicing capability including MIG profiles — served from the
observed status side and aggregated across clusters, so that what I display tracks reality after hardware or
mode changes without re-admitting InstanceType objects.

#### Story 3
As the **developer of the dynamic-MIG follow-up spec**, I want per-card structured mutual exclusion and a
group-level profile aggregate already in place, so that webhook validation, AdmissionCheck filtering, and the
Allocate path can be built against typed facts rather than parsing notes or hardcoding profile tables.

#### Story 4
As a **GPUStack operator packager**, I want the dead `--slicing-policy` knob and write-only fields gone, so the
deployment surface only exposes switches that do something.

### Core Features & Acceptance Criteria

#### F1 — Devices API restructure (`api/worker/v1alpha1/devices.go`)

New per-card types, extending the existing per-card `Status` (the `Accelerator` type already notes
"Field number 5 is reserved for the removed per-accelerator Features" — this spec effectively restores that
design in a new shape; T12 later closes that field-5 gap so `Accelerator` is contiguous, per the 2026-07-21
no-reservation decision):

```go
// AcceleratorLogicalSliced describes the card's logical (software) slicing capability.
type AcceleratorLogicalSliced struct {
    // CoresPercentageOvercommit reports whether each slice may claim up to 100% of the
    // device compute (time-sharing / weighted sharing); false means compute is partitioned.
    CoresPercentageOvercommit bool `protobuf:"varint,1,opt"`
    // Count is the maximum number of soft slices this card can host (formerly the group's
    // LogicalSliced.MaxSize). A MIG-enabled (or pending-enable) card is always 0.
    Count int32 `protobuf:"varint,2,opt"`
}

// AcceleratorPhysicalSlicedProfile describes one hardware partition profile of the card.
type AcceleratorPhysicalSlicedProfile struct {
    Name          string `protobuf:"bytes,1,opt"`  // e.g. "1g.5gb" — display and future resource-key suffix
    MemoryMib     int64  `protobuf:"varint,2,opt"` // e.g. 4864
    ComputeSlices int32  `protobuf:"varint,3,opt"` // 1..7 — the request granularity on the compute axis
    MemorySlices  int32  `protobuf:"varint,4,opt"` // 1..8 — the request granularity on the memory axis
    Count         int32  `protobuf:"varint,5,opt"` // max instances of this profile on one card (7/4/3/2/1/1)
}

// AcceleratorPhysicalSliced describes the card's physical (hardware) slicing capability.
type AcceleratorPhysicalSliced struct {
    // Profiles is empty when the card does not support, or has not enabled, hard slicing.
    Profiles []AcceleratorPhysicalSlicedProfile `protobuf:"bytes,1,rep"`
    // Count is the card's physical-slice ceiling — the largest Count across Profiles (e.g. 7 on
    // A100, from 7× 1g.5gb). It sizes the device-plugin's bare ".sliced" token pool for a
    // MIG-enabled card (F5), so a hard-partitioned card stays served in the ".sliced" pool
    // rather than dropping out. Zero when Profiles is empty.
    Count int32 `protobuf:"varint,2,opt"`
}

// AcceleratorStatus — existing type, extended (Unhealthy stays field 1).
type AcceleratorStatus struct {
    Unhealthy      bool                      `protobuf:"...,1"`
    LogicalSliced  AcceleratorLogicalSliced  `protobuf:"bytes,2,opt"`
    PhysicalSliced AcceleratorPhysicalSliced `protobuf:"bytes,3,opt"`
}
```

Group-level aggregate, replacing `AcceleratorsFeature` on `DevicesGroup` (the new field lands at 12 while
`AcceleratorsFeature` still occupies 11; T12 removes 11 and renumbers this field 12→11 so the group stays
contiguous):

```go
// AcceleratorSlicedLogicalDetail aggregates the group's logical slicing capability. The two
// levels serve different readers: the per-card LogicalSliced (F1) is what AdmissionCheck reads
// to decide a specific card; this group-level view is what external queries read to learn
// whether the node accepts soft-slice requests at all (Count > 0) and whether it permits compute
// overcommit (CoresPercentageOvercommit).
type AcceleratorSlicedLogicalDetail struct {
    // CoresPercentageOvercommit is a per-model property (uniform within a group by construction —
    // a group is one manufacturer+product+VRAM), so aggregation takes the flag from any
    // soft-sliceable card (LogicalSliced.Count > 0). When no card is soft-sliceable
    // (Count == 0, e.g. an all-MIG group) the flag is false and carries no meaning.
    CoresPercentageOvercommit bool
    Count                     int32 // Σ per-card LogicalSliced.Count (already includes the card dimension)
}

// AcceleratorSlicedPhysicalDetailProfile aggregates one profile across the group's cards.
type AcceleratorSlicedPhysicalDetailProfile struct {
    Name  string
    Count int32 // Σ per-card Count for this profile name
}

// AcceleratorSlicedPhysicalDetail aggregates the group's physical slicing capability.
type AcceleratorSlicedPhysicalDetail struct {
    Profiles []AcceleratorSlicedPhysicalDetailProfile
    Count    int32 // Σ per-card PhysicalSliced.Count (group-wide physical-slice ceiling, for reporting)
}

// AcceleratorSlicedDetail is the group-level slicing capability view.
type AcceleratorSlicedDetail struct {
    Logical  AcceleratorSlicedLogicalDetail
    Physical AcceleratorSlicedPhysicalDetail
}
```

Deletions: the `AcceleratorSliced` and `AcceleratorsFeature` types, the `MaxSlices()` and
`SlicedCoresOvercommit()` methods, and the `MemoryPercentageStep` field (written `=1` by all six detectors,
`=25` by the NVIDIA MIG placeholder, read by nothing — the profile table's `ComputeSlices`/`MemorySlices` now
express the request granularity exactly, one dimension richer than a scalar step). The `pkg/device/types.go`
aliases (`AcceleratorSliced`, `AcceleratorsFeature`) are replaced with aliases for the new types.

**Acceptance:**
- `make generate` regenerates CRDs/deepcopy/protobuf/applyconfigurations cleanly; every message renumbered by
  this spec (e.g. `InstanceTypeSpec`, `InstanceTypeStatus`, `DevicesGroup`) carries **contiguous** protobuf
  field numbers (1..N, gap-free) — reservation is dropped (2026-07-21 decision).
- `AcceleratorSlicedDetail` (contains a slice) never appears in `InstanceTypeSpec` or
  `InstanceTypeFlavorSpec` — both are map keys and must stay comparable; the detail rides only on
  status-side types.

#### F2 — Detector per-card reporting + group aggregation (all six vendors)

Today all six detectors write the group-level `LogicalSliced` once, in the "new group" branch
(`hygon/device.go:198`, `metax/device.go:148`, `ascend/device.go:226`, `mthreads/device.go:183`,
`cambricon/device.go:165`, `nvidia/device.go:183`). Change every detector to:

1. Fill each card's `Status.LogicalSliced` (same values they use today: e.g. NVIDIA
   `{Count: 128, CoresPercentageOvercommit: true}`) — per card, not per group-creation.
2. NVIDIA only: logical and physical are mutually exclusive per card, keyed on the **current** MIG mode
   (`dev.GetMigMode()` `migCurrent == DEVICE_MIG_ENABLE`). A currently MIG-enabled card sets **only**
   `PhysicalSliced` — profiles enumerated via NVML (`GetGpuInstanceProfileInfo` probing), filtered to the plain
   profiles (any `+…` variant dropped), filling `Name`/`MemoryMib`/`ComputeSlices`/`MemorySlices`/`Count` plus
   the `Count` ceiling — and leaves `LogicalSliced` empty. Every other card — MIG off, MIG **unsupported**
   (`GetMigMode` returns not-supported on non-MIG cards such as V100/T4/RTX, which **must** stay soft-sliceable),
   or the mode unreadable — sets **only** `LogicalSliced`. A pending-mode transition is not partitioned yet and
   is re-detected after the administrator's reset + DeviceManager restart. This replaces the placeholder seed at
   `nvidia/device.go:188-197` and fixes its first-card-only-seed defect (per-card logic runs for every card).
3. As the shared final step of `DetectAccelerator`, aggregate the group's `AcceleratorSlicedDetail` from its
   cards: `Logical.Count = Σ`; `Logical.CoresPercentageOvercommit` = the flag from any soft-sliceable card
   (it is uniform per model, so this is unambiguous; false when no card is soft-sliceable);
   `Physical.Profiles` summed by name.

**Acceptance:**
- Table-driven detector tests: a mixed group (2 non-MIG + 1 MIG-enabled NVIDIA card) yields per-card records
  with the structural mutual exclusion (`Count==0 ⟺ Profiles non-empty`) and a group detail of
  `Logical.Count == 256`, `Physical.Profiles` equal to one card's profile table.
- Keying on the current mode: a pending-enable card (`current==DISABLE`) is still soft-sliceable (logical); a
  pending-disable card (`current==ENABLE`) still reports physical; a `GetMigMode` error/not-supported card
  reports logical (a non-MIG-capable card such as V100/T4/RTX must never lose its soft-slice capability).

#### F3 — Remove `--slicing-policy`

Sites: `pkg/devicemanager/allocator/option.go` (default `:34`, flag `:48`, validation `:63-64`, `Complete`
`:75`), `allocator/config.go:13`, `allocator/allocator.go:28,50`, and `pkg/device/types.go:99-144`
(`AllocatorSlicingPolicy` type, both constants, `GetAllSlicingPolicies`). Zero of the nine vendor subpackages
read `Config.SlicingPolicy`; the priority it was meant to express (soft → virtual → physical) is now carried by
explicit resource-key semantics plus the per-card structural exclusion.

Deployment compatibility: cobra fails hard on an unknown flag, so a deployed DaemonSet still passing
`--slicing-policy` would crash-loop after upgrade. Default plan: verify the shipped manifests/charts under
`deploy/` never render the flag, then delete outright; if any shipped configuration renders it, keep a hidden
deprecated no-op flag for one release instead (Ask-first boundary).

Also update `specs/2026-06-25-accelerator-soft-slicing-runtime-isolation.md` (Non-Goals, line 65), which
recorded the `slicing-policy` plumbing as a deliberately preserved hook — mark that hook as removed by this
spec rather than still reserved.

**Acceptance:** grep per success criterion 4; DeviceManager starts cleanly with no slicing flags; the
2026-06-25 spec's stale wording is updated in the same change.

#### F4 — NodeCapacityReconciler re-plumb + Devices watch

`desiredSlicedCapacity` / `slicedFeatureByAcceleratableKey`
(`pkg/worker/controllers/worker/node_capacity.go:131-212`) currently read
`AcceleratorsFeature.MaxSlices()`/`SlicedCoresOvercommit()` and compute, per manufacturer:
`units = cards × D`, `cores = cards × maxSlices × 100` (overcommit) else `cards × 100`,
`memoryPct = cards × 100`, `memoryMib = cards × VRAM`.

New sourcing — index groups by `AcceleratorSlicedDetail`, and split the card population by slicing kind, because
`.sliced.units` (the universal Kueue quota unit, consumed by *both* logical and physical/MIG requests) must
count every sliceable card, while the three logical-specific keys must count only logically-sliceable cards
(the key-semantics split from F1: `.sliced.cores/memory-percentage/memory-mib` belong to logical slicing only).
Both counts are derived from per-card Devices data, not the raw `<nodeKey>.count` label:

```
sliceableCards = per group: count of cards where LogicalSliced.Count > 0 OR PhysicalSliced.Count > 0
softCards      = per group: count of cards where LogicalSliced.Count > 0        (logical only)

units      = Σ sliceableCards × D                   (full device, VRAM-anchored — MIG cards included)
cores      = Σ Detail.Logical.Count × 100           (overcommit; Count already includes the card dimension)
           = Σ softCards × 100                       (non-overcommit)
memoryPct  = Σ softCards × 100
memoryMib  = Σ softCards × per-card VRAM MiB
```

On a node with **no** MIG-enabled card, `sliceableCards == softCards == cards` and every value is numerically
identical to today (`Logical.Count = cards × maxSlices`, so `cores` matches the old `cards × maxSlices × 100`,
and `units = cards × D`). On a **MIG-enabled** node: `.sliced.units` still counts the MIG cards at full D each
(preserving today's behavior — today a MIG card counts via the group's 128-slice placeholder), while the three
logical keys shed the MIG cards (the truth-fix: a hard-partitioned card offers no logical soft-slice budget). A
group whose cards are all MIG-enabled still contributes to `.sliced.units` but nothing to the three logical
keys.

*Note:* splitting the two card counts is a refinement made after the maintainer's F5 correction — the earlier
draft wrongly dropped MIG cards from `.sliced.units` too. No API change beyond the F1 `PhysicalSliced.Count`
field; the reconciler already holds the full Devices object.

Add a `Watches(&workercore.Devices{})` clause: today the controller is `For(Node)` only
(`node_capacity.go:309-345`), with the bare `.sliced` pool as "the sole signal of a slicing-capability change".
MIG mode changes arrive via DeviceManager restart re-detection mutating
`Devices.Spec.Groups[].AcceleratorSlicedDetail` (and per-card statuses), which must enqueue the owning node.
Reuse the dedup-window mapping pattern the InstanceTypeReconciler already uses for its Devices watch
(`instance_type.go:591-607`), with a predicate limited to managed Devices whose spec slicing detail changed.

The `.sliced.mig-<profile>` capacity keys are **not** added here (deferred to spec 2 — advertising a key whose
data path does not exist yet is misleading).

**Acceptance:** table-driven reconciler tests covering (a) non-MIG group → four keys identical to the current
implementation's output for the same fixture; (b) mixed group → `.sliced.units` counts all sliceable cards
(logical + MIG), the three logical keys scale by `softCards` only; (c) all-MIG group → `.sliced.units` present
(MIG cards × D), the three logical keys reverse-patched to null; (d) a Devices-only change (no Node change)
converges the node capacity via the new watch.

#### F5 — Device-plugin token pool re-plumb (`pkg/deviceplugin/server.go:157`)

`getListAndWatchResponse` sizes each card's token pool with
`res.GetDeviceIds(s.AllocationMode, devGroup.AcceleratorsFeature.MaxSlices())` — the fourth `MaxSlices()`
consumer (`GetDeviceIds` uses the count only in Sliced mode, `helper.go:44`). Re-plumb to a **per-card**
effective sliced-token count: `LogicalSliced.Count` when the card is logically sliceable, else
`PhysicalSliced.Count` (the F1 physical ceiling). A small helper on `AcceleratorStatus` returns the non-zero one
(they are mutually exclusive by construction). The bare `.sliced` token pool is **preserved for MIG cards** —
they must stay served so a MIG allocation (spec 2) can land — sized to the physical ceiling instead of dropping
to zero.

Consequence: on a MIG-enabled node the per-card `.sliced` pool changes from today's 128 placeholder tokens to
the card's physical ceiling (e.g. 7 on A100) — a truth-fix, not a removal; the card stays in the pool. Non-MIG
nodes are unchanged (`LogicalSliced.Count == today's group MaxSlices`). Release-note the MIG-node token-count
change.

**Acceptance:** unit test on `getListAndWatchResponse` with a mixed fixture: non-MIG cards advertise
`LogicalSliced.Count` tokens each, the MIG-enabled card advertises `PhysicalSliced.Count` tokens (non-zero);
Exclusive/Shared/Visibility pools are unaffected.

#### F6 — Remove the `acceleratorFeature` flavor note and its consumers

Only the `acceleratorFeature` note is removed; the `cpuDetail` / manufacturer / product / family notes stay
(they feed F7's `Status.Detail` backfill). **Removal order matters** — the note producer must die only after its
last reader, or a new InstanceType in the gap reads as non-sliceable (breaking the non-MIG invariant):
- Flavor catalog projection (`extensionapis/worker/instance_type_flavor.go:411-423`): the `Sliceable` field
  loses its data source and is **removed** — dropped from `InstanceTypeFlavorSpec` (the read-only catalog row)
  and from `instanceTypeFlavorSpec`'s note parsing. This is a pure *reader* removal, safe to do early (plan T7).
  Slicing detail is instead surfaced through the InstanceType's `Status.Detail` (F7) / the WorkerGateway
  aggregate (F8). `InstanceTypeFlavorSpec` stays comparable (a field is removed, none added).
- `InstanceTypeWebhook.Default`: delete the `Spec.Feature` backfill block
  (`webhooks/worker/instance_type.go:193-204`) — the *last functional reader* of the note. Removed in plan T9,
  together with the `IsSliceable()` switch to `Status.Detail`.
- `NodeFlavorReconciler`: delete the note **write** (`node_flavor.go:218-219`) and the
  `nodeFlavorAcceleratorsFeature` helper (`node_flavor.go:325-347`) — the *producer*. Removed in plan T9 in the
  same change as its last reader above (not earlier). The wholesale note-replacement logic then sheds the stale
  note from existing ResourceFlavors on the next reconcile.

**Acceptance:** no `acceleratorFeature` string remains in non-test code after T9; at no intermediate step is the
note produced-but-unread or read-but-unproduced; the catalog builds and serves rows with no `Sliceable` field.

#### F7 — InstanceType desired/observed split

```go
type InstanceTypeSpec struct {
    // Admin-writable on Create and Update:
    DisplayName string // existing, +k8s:validation:maxLength=64
    // +k8s:validation:maxLength=1024
    Description string // NEW — free-form admin annotation
    Inactive    bool   // existing

    // Written only via Update after Create: the operator seeds these when it creates a derived
    // InstanceType; the admin edits them via Update. These stay on Spec (desired state) — the
    // reconciler never rewrites them (it only writes Status.Detail below).
    AcceleratorGroup string
    GeneralGroup     string
    Acceleratable    bool
    OS               string
    Arch             string
    UnitResources    InstanceTypeUnitResources
    LocalStorage     string
}
// Removed from Spec: Manufacturer, Product, Family, inline InstanceTypeCPU, inline
// InstanceTypeAccelerator. The remaining fields are reordered as above and renumbered
// contiguously (protobuf 1..10, gap-free) — no reservation (2026-07-21 decision).

// InstanceTypeDetail is the observed hardware descriptor, embedded in Status. It mirrors the
// old descriptor but carries the pool-aggregated SlicedDetail in place of the desired-state
// Feature, so the status-side detail can hold the slice-bearing AcceleratorSlicedDetail the
// comparable Spec must not.
type InstanceTypeDetail struct {
    Manufacturer string
    Product      string
    Family       string
    InstanceTypeCPU               // inline
    InstanceTypeAcceleratorDetail // inline; carries SlicedDetail AcceleratorSlicedDetail
}

// Reordered + renumbered contiguously (protobuf 1..8, gap-free): Detail first, then Entrance.
type InstanceTypeStatus struct {
    Detail       InstanceTypeDetail // field 1
    Entrance     string             // field 2
    Phase        string
    PhaseMessage string
    Accelerator, AcceleratorShared, AcceleratorSliced, CPU InstanceTypeResource
}
```

- `IsSliceable()` moves with `InstanceTypeAccelerator` into the Detail and re-judges on `SlicedDetail`
  (non-zero: `Logical.Count > 0 || len(Physical.Profiles) > 0`). Its three external callers re-plumb from
  `instType.Spec.IsSliceable()` to the status detail: `pkg/worker/controllers/worker/instance.go:947`,
  `pkg/worker/webhooks/worker/instance.go:305` and `:414`. Each caller must handle the empty-Detail window
  (below) gracefully.
- **Backfill moves entirely from the webhook to the reconciler.** All observed-hardware write-back detaches
  from `InstanceTypeWebhook.Default` and lands in the InstanceTypeReconciler — which already watches Devices
  with a 3s dedup window (`instance_type.go:591-607`). It fills `Status.Detail` via the `/status` subresource,
  sourced **exactly as the webhook fills the descriptor today**: manufacturer/product/family (and, for an
  accelerated type, memory/cores + CPU `cpuDetail`) from the **matched ResourceFlavor's notes** (the reconciler
  lists ResourceFlavors by the schedule labels, as the webhook does), plus the **Devices group's
  `AcceleratorSlicedDetail`** for the accelerator's `SlicedDetail`. This matters for **CPU-only / generic**
  types: they have no Devices group, so their Detail comes entirely from the ResourceFlavor `cpuDetail` note
  (kept by this spec) — "backfill from the Devices groups" alone would leave them without a Detail. Because
  `/status` writes bypass the admission webhooks (registered for the main resource's CREATE/UPDATE), the Detail
  write never hits `validateInstanceTypeSpecImmutable`. This absorbs DeviceManager-restart capability changes
  that the admission-time snapshot never could.
- **The empty-`Status.Detail` window is real and handled by a retryable contract, not hidden.** The entrance
  label is stamped by the mutating webhook at admission (`instance_type.go:141-143`) and the Pod webhook finds
  the InstanceType by that label (`pod.go:229-253`) and reads per-card VRAM synchronously — so between an
  InstanceType's creation and its first reconcile, that lookup sees an empty Detail. Gating the backing queue on
  Detail readiness narrows the *Kueue-admitted* path but does **not** stop the Pod webhook firing. The contract
  is therefore: **a read of an empty `Status.Detail` (VRAM or `IsSliceable`) is a retryable "instance type not
  ready" rejection** — never a whole-card fallback, a permanent non-sliceable verdict, or a panic. The window is
  bounded by one reconcile and self-heals on retry.
- **Webhook adjustments**: the Default webhook keeps label stamping, the GeneralGroup default, and the
  entrance label, and drops the descriptor-enrichment block (`instance_type.go:160-212`) — no observed data is
  written at admission anymore. The DisplayName default (was set at admission from `Spec.Product`) moves to the
  **NodeFlavorReconciler's derived authoring**, which stamps `Spec.DisplayName` from the flavor's product (or the
  `"CPU-only"` sentinel for the CPU-manufacturer-agnostic collapsed pool) once at creation. An admin-created type
  is not auto-named. DisplayName is admin-editable, so a later rename is preserved; the InstanceTypeReconciler
  makes no Spec write (it only writes `Status.Detail`).
  `validateInstanceTypeSpecImmutable` (`instance_type.go:328-337`) adds `Description` to the admin-editable masked
  set (DisplayName + Description + Inactive).
- **Migration**: existing stored InstanceTypes carry the removed spec fields; after the CRD update those
  fields prune on the next write, and the reconciler populates `Status.Detail` on its first pass. v1alpha1
  in-place type changes follow the project's prior refactor precedent; call the upgrade note out in the
  release notes.

**Acceptance:** a freshly derived InstanceType has Spec containing only the retained fields, gains
`Status.Detail` within one reconcile, and `validateInstanceTypeSpecImmutable` still freezes everything except
DisplayName/Description/Inactive for the admin; the three `IsSliceable` call sites behave identically for a
populated Detail and fail safe (treat as not-sliceable-yet, not panic) for an empty one.

#### F8 — WorkerGateway aggregation (`pkg/workergateway/service/types.go`)

- `AggregatedInstanceTypeOnceMaxRequestCandidate` (`:119-143`) gains
  `AcceleratorSlicedDetail workercore.AcceleratorSlicedDetail` (from the cluster InstanceType's
  `Status.Detail`).
- `AggregatedInstanceTypeOnceMaxRequestTier` (`:96-116`) becomes
  `{OnceMaxRequest, Remaining, Candidates, AcceleratorSlicedDetail}` — a tier groups candidates by accelerator
  `OnceMaxRequest` and carries **no identity descriptor**; its `AcceleratorSlicedDetail` is the direct Σ of its
  candidates (same-name profile Counts added).
- `AggregatedInstanceTypeStatus` (`:51-77`) becomes `{Detail, OnceMaxRequest, Remaining, Tiers}`. `Detail` is
  the **single place the observed descriptor lives** — tiers group candidates and candidates are the per-cluster
  members, neither carries identity. `Detail.SlicedDetail` is the direct Σ of every tier's slicing capability.
- `Recompute` aggregates level-by-level, mirroring the detector's card→group aggregation. **The slicing
  aggregation is pure direct summation** (not the winning-candidate/representative selection that
  `OnceMaxRequest` uses): every profile `Count` is summed by name across candidates into the tier
  (`tier.AcceleratorSlicedDetail`), and across tiers into `Status.Detail.SlicedDetail`. Counts are summed
  regardless of candidate `Phase` — it is a hardware capability descriptor, not a live requestable resource.
- `Status.Detail`'s **identity fields** (manufacturer/product/family/CPU/per-card memory·cores) are uniform
  across an item's candidates (same hardware) and are **maintained at the status level, adopted at ingestion**
  from any reconciled candidate (`Detail.AcceleratorReady()`, i.e. non-empty Manufacturer). This self-heals a
  descriptor first seen during the pre-reconcile window: as soon as any candidate reports its hardware (a
  co-tier ready candidate, or the original candidate's own reconcile `Modified`), the identity populates.
  `Recompute` owns only `Detail.SlicedDetail` and never overwrites the adopted identity.
- `AggregatedInstanceTypeSpec` is an alias of `workercore.InstanceTypeSpec` (`:44`): every consumer that read
  Manufacturer/Product/Family off the aggregated Spec must switch to `Status.Detail` (coordinate with UI/API
  consumers; the gateway REST shape changes). Post-T10 the aggregated Spec no longer carries those fields, so no
  in-tree reader survives — the F8 checklist verifies this and re-sources any needed descriptor from
  `Status.Detail`.

**Acceptance:** `Recompute` unit tests — two clusters with the same profile name sum Counts at tier
(`tier.AcceleratorSlicedDetail`) and top level (`Status.Detail.SlicedDetail`); the status descriptor identity
self-heals from a not-yet-reconciled first candidate once a reconciled candidate arrives; a consumer-visible
JSON fixture documents the new shape (`Detail` only on the item status; standalone `AcceleratorSlicedDetail` on
each tier and candidate).

#### F9 — MIG manual-lifecycle operations documentation

A docs page (under `docs/`) delivering the administrator sequence, with the profile/placement tables above
copied in:

1. **Enable**: drain/stop accelerator clients on the node as needed → `nvidia-smi -i <id> -mig 1` (or all
   cards) → Ampere: GPU reset required (all driver-handle daemons stopped first; `nvidia_drm` loaded or
   passthrough-VM reset restrictions may force a node/VM reboot); Hopper+: no reset → restart the node's
   DeviceManager pod → verify the Devices ledger shows the card's profiles and zero soft capability.
2. **Disable**: inverse sequence (`-mig 0`, same reset rules, DeviceManager restart).
3. **No automatic descheduling — the administrator owns Pod lifecycle around MIG changes.** The doc states
   plainly that GPUStack never evicts or deschedules a Pod in response to a MIG mode change. In practice the
   awkward case cannot arise: a card cannot have its MIG mode reset while a Pod is using one of its instances
   (the reset requires the card's instances to be idle), so an administrator changing the setting must stop the
   using Pod first anyway.
4. **Node reboot recovery**: MIG instances never survive a reboot, and on Hopper+ the mode itself does not
   persist. If the administrator did not reset before the reboot: on the way back up the instances (and, on
   Hopper+, the mode) are gone, so the administrator redoes the enable sequence and restarts the DeviceManager,
   which re-detects and realigns the Devices ledger to the actual post-reboot hardware. Whether an
   already-allocated Pod's instance can then be *recreated* depends on the ledger realigning with what that Pod
   holds — that recreate capability, and the exact Pod-state/ledger interaction on restart, are the follow-up
   allocation spec's job (spec 1 only guarantees the ledger reflects reality after re-detection).
5. Explicit statement of what the operator does **not** do: no nodeconfig/label triggers, no automatic mode
   flips, no geometry rewrites, no descheduling; capability changes enter only via DeviceManager restart or
   detector re-detection.

**Acceptance:** doc exists, sequences match the grounding facts above, states the no-auto-descheduling policy
and the stop-Pod-before-reset expectation explicitly, and uses no cloud-provider or host-specific naming (a
generic "a Kubernetes cluster" wording).

### Notes / Constraints / Caveats

- **Comparability is a hard constraint**: `InstanceTypeSpec` and `InstanceTypeFlavorSpec` are used as map
  keys; any field added to them must be a comparable value type. `AcceleratorSlicedDetail` contains slices and
  therefore lives only on status-side types (`Devices` group spec is not a map key; `InstanceTypeDetail` is
  status-only).
- **Protobuf discipline (2026-07-21 decision)**: this spec drops field-number reservation and instead renumbers
  every message **contiguously** (natural-number ordering, no gaps) as fields are removed; `protobuf_reserved_test.go`
  is deleted (T10). This is a pre-release wire break accepted deliberately.
- **`make generate` after any `api/` or webhook change** (the `gpustack-operator-generate` skill); it must run
  from the main checkout — `go-to-protobuf` requires the working directory to end in `gpustack.ai/gpustack`.
- **Behavior invariance** on non-MIG hardware is a review gate for every re-plumb site: same inputs, same
  advertised numbers.
- Where a task in this spec conflicts with wording in earlier shipped specs
  (`2026-06-25-accelerator-soft-slicing-runtime-isolation.md` on `slicing-policy`;
  `2026-07-16-accelerator-slicing-capability-and-pool-feedback.md` on detector `LogicalSliced` settings), this
  spec supersedes them via the **"Supersedes prior design" declaration in this spec's header**, leaving the
  archived specs untouched rather than silently contradicting or editing them.

### Boundaries

- **Always:** run `make generate` from the main checkout after API/webhook edits; run `make lint` (long
  timeout on cold cache) and `go test ./...` before considering a task done; sign off every commit
  (`--signoff`); keep `InstanceTypeSpec`/`InstanceTypeFlavorSpec` comparable; preserve numeric behavior on
  non-MIG nodes; keep docs cloud-provider-generic.
- **Ask first:** hard-deleting `--slicing-policy` if any shipped manifest/chart renders it (vs. a one-release
  deprecated no-op); any change that alters Kueue quota values or the Pod webhook's alignment math; touching
  the gateway REST shape beyond the F8 list.
- **Never:** automate MIG mode switching in any controller; add a memory→profile translation layer; enable
  `.sliced.mig-*` capacity keys in this spec; leave a protobuf message with gap-numbered fields (renumber
  contiguously instead); reference research working files from code or docs.

### Risks and Mitigations

- **Stored-object migration (v1alpha1 in-place change)** — correctness does **not** depend on old fields being
  pruned: after T9/T10 no code reads `Spec.Manufacturer`/`Memory`/`Feature` etc., so a stored object's stale
  spec JSON is simply ignored. A `/status` backfill does not rewrite `spec`, so those fields linger until the
  next main-resource write (cosmetic) — do not rely on pruning as the migration mechanism. Mitigation: readers
  switch to `Status.Detail` (T9) before the fields are removed (T10); the reconciler repopulates `Status.Detail`
  on its first pass; a round-trip test asserts reads work with both old-shaped and new-shaped stored objects;
  release-note the upgrade ordering (operator first, then DeviceManager DaemonSet).
- **Empty `Status.Detail` window at admission** — the entrance label is stamped by the webhook *before* any
  reconcile, and the Pod webhook fires on that label regardless of queue existence (`instance_type.go:141-143`,
  `pod.go:229-253`), so a queue-readiness gate does **not** hide the window (Codex-refuted). Mitigation: every
  Detail reader (Pod-webhook VRAM, `IsSliceable` at the three call sites) treats empty Detail as a **retryable
  "not ready" rejection** — never a whole-card fallback, permanent non-sliceable verdict, or panic; the window
  is one reconcile and self-heals. Covered by explicit tests (direct Pod + Instance-created Pod).
- **CPU-only Detail has no Devices source** — a CPU-only/generic InstanceType has no Devices group, so a
  Devices-only backfill would leave it forever unready → F7 sources Detail from the matched ResourceFlavor's
  kept notes (`cpuDetail`/manufacturer/…) for the CPU part and Devices only for `SlicedDetail`; readiness is
  defined per type (CPU Detail for CPU-only, accelerator Detail for accelerated).
- **Note producer/reader removal ordering** — removing the `acceleratorFeature` producer before its webhook
  reader would make new types read non-sliceable mid-rollout (Codex Critical) → F6/T9 remove producer and last
  reader in the same change; the catalog `Sliceable` reader (T7) is removed earlier only because dropping a
  reader is always safe.
- **Protobuf field-number reuse** — `generated.proto` carries no `reserved` declaration and a hand-added one is
  clobbered by `make generate`, so reservation cannot be enforced durably (Codex R11). **Resolved (2026-07-21)
  by dropping reservation entirely:** every message is renumbered **contiguously** (natural-number ordering) as
  fields are removed, so there are no reserved numbers to protect and `protobuf_reserved_test.go` is deleted
  (T10). This is a deliberate pre-release wire break; no client depends on the old numbers yet.
- **Flag removal crash-loops deployed DaemonSets** → F3's deployment-compatibility gate (verify `deploy/`,
  else deprecation shim; Ask-first).
- **Node-vs-Devices cardinality skew** — F4 changes the card-count source from the Node `.count` label (built
  from `len(group.Accelerators)`) to the Devices per-card data. On settled data they match; under stale/
  out-of-order updates they can differ → the Devices ledger is the single source of truth for all F4 counts
  (do not mix with the label), and a test covers a label-vs-Devices skew fixture asserting Devices wins. This
  also keeps `sliceableCards`/`softCards` internally consistent.
- **`.sliced` token / logical-key change on MIG-enabled nodes reads as a regression** → on a MIG node the bare
  `.sliced` pool re-sizes from the 128 placeholder to the physical ceiling (F5) and the three logical keys shed
  MIG cards (F4), while `.sliced.units` and pool presence are preserved; it is the intended truth-fix.
  Release-note the before/after explicitly (non-MIG nodes are unchanged).
- **Watch fan-out from the new Devices watch** → reuse the existing 3s dedup-window enqueue pattern; the
  predicate fires only on managed Devices whose spec slicing detail changed.
- **Gateway shape change breaks UI consumers** → coordinate the `Status.Detail` move with the consuming
  UI/API before merging F8; the alias `AggregatedInstanceTypeSpec` keeps compiling either way, so the break is
  behavioral (empty fields), not compile-time — an explicit consumer checklist is part of the F8 task.

## Design Details

### Commands

**Environment: local `darwin`.** The whole module — including the CGO vendor detectors (`binding/nvml` etc.) —
builds and unit-tests locally on this machine; no Linux host or vendor SDK is needed for Go verification (only
the container image build would need one, which is out of scope here). Confirmed by a read-only smoke build of
`./api/worker/...` and `./pkg/devicemanager/detector/nvidia/...` during planning. There is **no NVIDIA hardware
here**, so the NVML MIG-profile enumeration cannot run locally; the profile derivation/filter logic is factored
into a pure function verified by table tests (real-card validation is spec 2 / external).

```bash
# Code generation (CRDs, deepcopy, protobuf, applyconfigurations) after any api/ or webhook change.
# MUST run from the main checkout — go-to-protobuf needs the cwd path to end in gpustack.ai/gpustack
# (it fails inside a git worktree):
make generate            # or the gpustack-operator-generate skill

# Build & test:
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./...
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test ./...
# Scope a fast inner loop to the packages a task touches, e.g.:
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test ./pkg/worker/controllers/worker/... ./pkg/devicemanager/detector/...

# Lint (whole-module golangci-lint; cold cache blows a 2-min timeout, ~20s warm — use a long timeout
# or run it in the background):
make lint
```

### Project Structure (touched surface)

```
api/worker/v1alpha1/devices.go            # F1: per-card + group slicing types; deletions
api/worker/v1alpha1/instance_type.go      # F7: Spec slim-down, Description, InstanceTypeDetail, Status.Detail
pkg/device/types.go                       # F1 aliases; F3 policy type/constants removal
pkg/devicemanager/detector/{nvidia,hygon,metax,cambricon,mthreads,ascend}/device.go
                                          # F2: per-card reporting + shared aggregation; NVIDIA NVML profiles
pkg/devicemanager/allocator/{option,config,allocator}.go   # F3: --slicing-policy removal
pkg/deviceplugin/server.go                # F5: per-card token pool
pkg/worker/controllers/worker/node_capacity.go   # F4: sourcing + Devices watch
pkg/worker/controllers/worker/node_flavor.go     # F6: note removal; F7: derived DisplayName stamp
pkg/worker/controllers/worker/instance_type.go   # F7: Status.Detail backfill
pkg/worker/controllers/worker/instance.go        # F7: IsSliceable re-plumb
pkg/worker/webhooks/worker/instance_type.go      # F6/F7: drop enrichment; immutability mask
pkg/worker/webhooks/worker/instance.go           # F7: IsSliceable re-plumb (two sites)
pkg/worker/extensionapis/worker/instance_type_flavor.go  # F6: remove Sliceable field
pkg/workergateway/service/types.go + Recompute impl      # F8
specs/2026-06-25-accelerator-soft-slicing-runtime-isolation.md  # F3: stale hook wording
docs/                                     # F9: MIG operations page
```

### Code Style

API types follow the existing `devices.go` conventions — exported fields with behavior-stating doc comments,
json/yaml/protobuf tags, `// nolint: lll` where a tag line runs long:

```go
// Count is the maximum number of soft slices this card can host. A currently
// MIG-enabled card is always 0, which excludes it from the three logical
// capacity keys; its ".sliced" token pool is sized from PhysicalSliced.Count instead.
Count int32 `json:"count,omitempty" yaml:"count,omitempty" protobuf:"varint,2,opt,name=count"`
```

Multi-word Go files in snake_case; table-driven tests with a shared execution loop; fake clients; assert
observable state (advertised capacities, ledger content), not implementation details.

### Implementation Plan

**Dependency graph & strategy.** The removed symbols (`AcceleratorsFeature`, `MaxSlices()`,
`SlicedCoresOvercommit()`, `MemoryPercentageStep`, the `acceleratorFeature` note) have ~6 consumers across
detectors, node_capacity, device-plugin, node_flavor, webhooks, extension API, and the InstanceType/gateway
types. Deleting them up front would break the build in many places at once. We therefore use a **strangler
order**: introduce the new types alongside the old (T3), populate both from the detectors (T4), migrate each
consumer off the old symbols on its own vertical path (T5–T11), then delete the orphaned old symbols last (T12).
The tree builds and tests green at every checkpoint. `--slicing-policy` removal (T2) and the docs (T13) are
independent and parked at the ends.

The riskiest unknown — exactly which NVML GI profiles to keep and their canonical names (`1g.10gb` is
`GPU_INSTANCE_PROFILE_1_SLICE_REV1`, a keeper, **not** a `+me` variant to drop) — is de-risked first (T1) as a
pure, fully-tested function, since there is no local NVIDIA hardware and no NVML mock seam in the detector.

**Phase A — De-risk & independent warm-ups**

[x] **T1 (PoC / de-risk): NVML MIG profile enumeration primitive + pure derivation-and-filter function.**
    - Add the `AcceleratorPhysicalSlicedProfile` API type (T1's output contract) to `devices.go` + its
      `pkg/device` alias, and run `make generate` (the remaining F1 family lands in T3). Splitting the one type
      T1 must return keeps generate clean at every commit; no duplicate local type.
    - Add a device-level accessor to `binding/nvml/library_device.go` —
      `func (l Device) GetGpuInstanceProfileInfo(profileId uint32) (GpuInstanceProfileInfo_v3, Return)` —
      wrapping the already-bound cgo call (`nvmlDeviceGetGpuInstanceProfileInfoV`/legacy), reachable from the
      detector's package (the existing handler hangs off `GpuInstance`, whose fields are unexported). Try
      V3→V2→V1 (V2/V3 carry the `Name`). Extract the `Name [96]int8` via a
      `(*GpuInstanceProfileInfo_v3).GetName()` method using `C.GoString` — the codebase idiom for a fixed
      C-string field (cf. `SMluInfo.GetInstanceName`), not a hand-rolled byte loop.
    - Add a pure function (in the nvidia detector package, no NVML I/O) that takes a slice of
      `GpuInstanceProfileInfo_v3` (one per supported profile id `0..GPU_INSTANCE_PROFILE_COUNT`) and returns the
      filtered `[]AcceleratorPhysicalSlicedProfile`: keep the plain profiles, drop any `+…` variant, derive
      `Name` (from the NVML `Name` field when present, else a `<slices>g.<mem>gb` fallback), `MemoryMib` =
      `MemorySizeMB`, `ComputeSlices` = `SliceCount`, `MemorySlices` = derived from `MemorySizeMB` vs the card's
      per-slice VRAM, `Count` = `InstanceCount`.
    - Filter predicate — **the NVML `Name` suffix is authoritative; profile IDs are NOT a stable
      cross-generation taxonomy** (cross-check R8): on A100 base ids 0/1 are the plain profiles, but on H100 the
      base ids are the `+me` variants and the `_NO_ME` ids (13/14) are the plain keepers — so a static "drop ids
      13-16" rule keeps exactly the wrong ones. Primary rule: **drop a profile whose NVML `Name` carries any
      `+…` variant suffix** (`+me`, `+me.all`, `+gfx`) — only the plain `<C>g.<M>gb` profiles are kept, so the
      discriminator is the presence of a `+` (V3 also exposes a GFX bit in `Capabilities`, `const.go:1067`, as a
      backstop); keep everything else, including `_REV1`/`_REV2` (`GPU_INSTANCE_PROFILE_1_SLICE_REV1` id 7 = `1g.10gb`,
      confirmed by Kimi's live probe and Codex). Probe every id `0 ≤ id < GPU_INSTANCE_PROFILE_COUNT` (17) and
      skip NVML "not supported"/"invalid argument" returns; never treat a zero-valued struct as supported.
      **Deduplicate by final name before the group sums profiles** (F2), so an unfiltered `+me`/plain pair that
      resolves to the same geometry name can never double-count. V1 (no `Name`) fallback: keep ids 0-9, drop
      10-16 — safe only because `+me`/`+gfx` exist only on Hopper/Blackwell, whose drivers carry the `...V`
      symbol (so V2/V3 is used there); document that residual edge. (Also: the earlier claim that
      k8s-device-plugin `mixed` "excludes +me/+gfx" is not reliable — `mixed` advertises
      `nvidia.com/mig-1g.10gb+me` as its own resource; dropping `+me` here is a deliberate product choice, not a
      cited precedent.)
    - Derivation (from Kimi's probe): `MemoryMib = int64(MemorySizeMB)` **verbatim** — NVML's "MB" is already
      MiB-scale (e.g. `4864` for `1g.5gb`; the marketing "5" in the name intentionally differs — do not
      "correct" it). `ComputeSlices = SliceCount`; `Count = InstanceCount`. `MemorySlices =
      round(MemorySizeMB / (cardMemoryMiB / 8))` — **round, not floor** (4864/5120 = 0.95 → 1). Fallback name
      when `Name` is absent: `<SliceCount>g.<MemorySlices × cardMemoryGiB/8>gb` (derive from geometry, never
      from `MemorySizeMB/1024`, which would corrupt it).
    - Acceptance: table tests feed the full A100-40GB probe set (ids 0-4 + 7 supported; 5,6,8,9,10-16 return
      errors) and assert exactly the six-row table (`1g.5gb`×7, `1g.10gb`×4, `2g.10gb`×3, `3g.20gb`×2,
      `4g.20gb`×1, `7g.40gb`×1) with `1g.10gb` present (id 7); an **H100 id set** (0=`+me`, 13=plain,
      15=ALL_ME `1g.10gb+me.all`, 10-12=GFX) asserting plain kept and `+me`/`+me.all`/`+gfx` dropped with no
      name-collision double-count; a
      V1-without-Name case asserting geometry-derived names; a `MemorySlices` rounding case. Pin the expected
      table to the NVIDIA-documented values, **not** the mig-parted mock (which gets this mapping wrong).
      Verify: `go test ./pkg/devicemanager/detector/nvidia/... ./binding/nvml/...` green; `go build ./...`
      green (accessor is additive).

[x] **T2 (independent): remove `--slicing-policy`.**
    - Delete the flag/field/validation/`Complete` wiring (`allocator/option.go:23,34,48,63-64,75`,
      `allocator/config.go:13`, `allocator/allocator.go:28,50`) and `pkg/device/types.go:99-144`
      (`AllocatorSlicingPolicy`, both constants, `GetAllSlicingPolicies`).
    - Deployment-compat gate: grep `deploy/` for `slicing-policy`; if unrendered, delete outright; if rendered,
      keep a hidden deprecated no-op flag for one release (Ask-first — see Boundaries).
    - Update `specs/2026-06-25-accelerator-soft-slicing-runtime-isolation.md:65` to mark the hook removed.
    - Acceptance: `grep -r "slicing-policy\|SlicingPolicy\|GetAllSlicingPolicies"` over non-generated,
      non-test Go returns nothing (modulo the optional shim); DeviceManager starts with no slicing flag.
      Verify: `go build ./...`, `go test ./pkg/devicemanager/...`.

**Phase B — New capability data model (additive)**  ·  *Checkpoint after T4: data flows end-to-end, nothing
consumes it yet; full build + test green.*

[x] **T3: add the new API types (F1), keep the old.**
    - In `api/worker/v1alpha1/devices.go`: add `AcceleratorLogicalSliced`,
      `AcceleratorPhysicalSliced` (incl. `Count`) — `AcceleratorPhysicalSlicedProfile` already landed in T1 —
      extend `AcceleratorStatus` with `LogicalSliced` /
      `PhysicalSliced` (new field numbers), add the group `AcceleratorSlicedDetail` family and a new
      `DevicesGroup.AcceleratorSlicedDetail` field (new number). Do **not** remove `AcceleratorsFeature` /
      `AcceleratorSliced` / `MaxSlices()` / `SlicedCoresOvercommit()` / `MemoryPercentageStep` yet.
    - Add `pkg/device/types.go` aliases for the new types.
    - Run `make generate`.
    - **Protobuf reservation guard (interim — retired in T10):** T3 added a **tag-audit test**
      (`protobuf_reserved_test.go`) parsing the Go struct tags to assert removed protobuf numbers stayed unused
      (`Accelerator` 5, `InstanceTypeAccelerator` 4). **Superseded by the 2026-07-21 decision:** the spec drops
      reservation entirely and renumbers every message **contiguously** (natural-number ordering) as fields are
      removed, so T10 deletes this test rather than growing it. Retained here as the record of what T3 built.
    - Acceptance: `make generate` clean; `go build ./...` green (new types unused). Verify: regenerated
      deepcopy/protobuf/applyconfig compile.

[x] **T4: all six detectors report per-card + aggregate the group detail (F2), NVIDIA wires T1.**
    - Each of `nvidia/hygon/metax/cambricon/mthreads/ascend/device.go`: set each card's
      `Status.LogicalSliced` (same values as today's group `LogicalSliced`); as a shared final step of
      `DetectAccelerator`, aggregate the group `AcceleratorSlicedDetail` (Logical.Count = Σ, overcommit from any
      soft-sliceable card, Physical.Profiles summed by name, Physical.Count = Σ per-card ceiling). **Keep writing
      the old `AcceleratorsFeature`** so existing consumers still work.
    - NVIDIA additionally: key on the **current** MIG mode (`GetMigMode()` `migCurrent == DEVICE_MIG_ENABLE`).
      A currently MIG-enabled card sets **only** `PhysicalSliced` (`Profiles` via the T1 enumeration over
      `0..GPU_INSTANCE_PROFILE_COUNT`, `Count` = max profile Count) and no `LogicalSliced`; replaces the
      placeholder at `device.go:188-197` and its first-card-only-seed defect (per-card loop now). **Every other
      card sets only `LogicalSliced`** — MIG off, MIG unsupported, or the mode unreadable. Keying on `migCurrent`
      (not pending, and NOT an error fail-safe) is deliberate (supersedes the earlier R5 "exclude on error"):
      `GetMigMode` returns not-supported on every non-MIG NVIDIA card (V100/T4/RTX), so treating error/unsupported
      as "exclude from the soft pool" would wrongly strip soft slicing from all of them — the common case. A
      pending-mode transition is not partitioned yet and is re-detected after the admin's reset + DeviceManager
      restart.
    - Acceptance: per-vendor table tests — a card carries `LogicalSliced`; a NVIDIA mixed group (2 non-MIG + 1
      MIG-enabled) yields the mutual exclusion (`Count==0 ⟺ Profiles non-empty`), group `Logical.Count == 256`,
      `Physical.Profiles` = one card's table, `Physical.Count == 7`. Verify: `go test ./pkg/devicemanager/...`.

**Phase C — Migrate the node/flavor consumers**  ·  *Checkpoint after T7: non-MIG numeric identity proven;
node_capacity, device-plugin, and the flavor note all off the old symbols.*

> **Cross-binary rollout skew (cross-check R1, Critical) — applies to T5 & T6.** The Devices CR is written by
> the per-node DeviceManager DaemonSet and read by the operator (node_capacity) and the on-node device-plugin.
> A rolling upgrade updates the operator before every DaemonSet, so the new node_capacity will read
> **old-format** Devices (only `AcceleratorsFeature`, no per-card data / `AcceleratorSlicedDetail`) from
> not-yet-upgraded nodes. A naive new reader finds no detail → skips the group → `desiredSlicedCapacity` returns
> nil → `buildSlicedCapacityPatch` reverse-patches all four `.sliced.*` keys to null, and the device-plugin
> token pool drops to 0 — **fleet-wide sliced-admission failure until each node's DaemonSet restarts**. The
> in-process strangler does nothing for this cross-binary skew. **Mitigation:** T5 and T6 read the new fields
> with a **fallback to `AcceleratorsFeature`/`MaxSlices()` when the new per-card/detail fields are absent** (old
> data ⇒ preserve today's numbers, never null). During the build itself the fallback keeps the four keys / token
> pool stable whenever a new reader meets old-format Devices.
>
> **Decision (2026-07-21): the fallback is a within-spec transient; `AcceleratorsFeature` is deleted in T12.**
> The maintainer confirmed the cross-binary skew is not a concern for the deployment model, so the R1 fallback
> is **not** kept a release. **T12 (below) deletes the whole `AcceleratorsFeature` family within this spec** —
> the fields, the `AcceleratorSliced`/`MemoryPercentageStep` types, `MaxSlices()`/`SlicedCoresOvercommit()`, the
> T4 dual-write in every detector, and the T5/T6 fallback branches — so `AcceleratorsFeature` is gone by T13.
> Accepted consequence: during an operator-ahead-of-DaemonSet upgrade window, a node still serving old-format
> Devices advertises no `.sliced.*` capacity (and a 0 token pool) until its DaemonSet restarts onto the new
> per-card format. T5/T6 keep the fallback only as a build-time transient until T12 removes it.

[x] **T5: NodeCapacityReconciler → new aggregate + per-card counts + Devices watch (F4).**
    - Re-source `desiredSlicedCapacity` / `slicedFeatureByAcceleratableKey` (`node_capacity.go:131-212`) from
      `AcceleratorSlicedDetail` and per-card data: `sliceableCards` (logical∨physical) drives `.sliced.units`;
      `softCards` (logical only) drives the three logical keys; overcommit from `Detail.Logical`. **All counts
      come from the Devices ledger as the single source of truth** — not mixed with the Node `.count` label
      (R-skew); resolve the model via the label key only to *find* the group.
    - **R1 dual-read fallback:** when a group has no per-card/`AcceleratorSlicedDetail` data (old-format Devices
      from a not-yet-upgraded DaemonSet during rollout), fall back to today's `AcceleratorsFeature`/`MaxSlices()`
      computation so the four keys keep their current values instead of nulling. Build-time transient only —
      removed in T12 (2026-07-21 decision: `AcceleratorsFeature` is deleted within this spec, not kept a release).
    - **VRAM source (R4):** the per-card VRAM for `.sliced.memory-mib` must stay the **lossy `.memory` label**
      (`quantityx.Format`, rounds to Gi) exactly as today — do **not** switch to the exact `DevicesGroup.Memory`
      uint64, or an ECC-restored non-Gi-aligned size (e.g. 43238 MiB) changes the advertised value and desyncs
      the Pod-webhook anchor.
    - Add `Watches(&workercore.Devices{})` with a managed-Devices predicate + 3s dedup window (mirror
      `instance_type.go:591-607`); the predicate fires on a **spec slicing-detail** change only, not on
      Status/allocation churn (R7).
    - Acceptance: F4 acceptance list + an **old-format Devices fixture** (only `AcceleratorsFeature`) asserting
      the fallback preserves the four keys (not null); a **non-Gi-aligned VRAM fixture** (43238 MiB) asserting
      `.sliced.memory-mib` matches today's lossy-label value; a **label-vs-Devices skew** fixture asserting
      Devices wins for cardinality. Verify: `go test ./pkg/worker/controllers/worker/...`.

[x] **T6: device-plugin `.sliced` token pool → per-card count (F5).**
    - Replace `devGroup.AcceleratorsFeature.MaxSlices()` at `server.go:157` with a per-card helper on
      `AcceleratorStatus` returning `LogicalSliced.Count` (logical) or `PhysicalSliced.Count` (MIG). **R1
      fallback:** when the per-card fields are absent (old-format Devices), fall back to the group
      `MaxSlices()` so the token pool is not zeroed mid-rollout. (Within a DaemonSet pod the detector and
      device-plugin upgrade atomically, so this fallback mainly guards a brief pod-restart transient.)
    - Acceptance: `getListAndWatchResponse` test per F5 acceptance (non-MIG = `LogicalSliced.Count`, MIG =
      `PhysicalSliced.Count`, other modes unaffected) + an old-format fixture asserting the fallback token count.
      Verify: `go test ./pkg/deviceplugin/...`.

[x] **T7: remove the flavor-catalog `Sliceable` reader only (F6, partial).**
    - Remove `Sliceable` from `InstanceTypeFlavorSpec` and its note parsing
      (`extensionapis/worker/instance_type_flavor.go:411-423`); confirm no catalog consumer reads it (broad
      grep). Removing a *reader* is always safe.
    - **Do NOT remove the `acceleratorFeature` note producer here.** The note is still read by the InstanceType
      mutating webhook (`instance_type.go:193-203`, which backfills `Spec.Feature`, which `IsSliceable()`
      depends on) until T9 switches those readers to `Status.Detail`. Removing the producer now would leave a
      new InstanceType with an empty `Spec.Feature` → read as non-sliceable → **breaks the non-MIG invariant
      during the T7→T9 window** (cross-check: Codex Critical). The producer removal is therefore folded into T9,
      alongside its last reader.
    - Acceptance: catalog builds/serves without `Sliceable`; the `acceleratorFeature` note is still produced and
      still consumed by the webhook (unchanged). Verify: `go build ./...`,
      `go test ./pkg/worker/extensionapis/...`.

**Phase D — InstanceType desired/observed split (F7)**  ·  *Checkpoint after T10: Spec slimmed, all readers on
`Status.Detail`, immutability + comparability intact.*

[x] **T8: add `InstanceTypeDetail` + `Status.Detail` + `Description`; reconciler backfills (keep Spec fields).**
    - Add `InstanceTypeDetail` (Manufacturer/Product/Family + inline CPU + inline Accelerator whose `Feature`
      becomes `SlicedDetail AcceleratorSlicedDetail`), `Status.Detail`, and `Spec.Description`
      (`+k8s:validation:maxLength=1024`). Do **not** remove the Spec descriptor fields yet (readers still
      compile). `make generate`.
    - **Compute `Detail` inside `computeStatus`, not via a separate `/status` write (R7).** The reconciler
      assigns the whole status wholesale (`it.Status = desiredStatus`, `instance_type.go:123-131`), so a
      separately-written Detail would be stomped on the next pass. Fold Detail into `computeStatus` and derive it
      from `instanceTypeScheduleLabels(it)` (`:371-377`) — **not** from the ClusterQueue's labels (`:354`,
      `:383-393`) — so Detail is computable **before** the CQ exists (the readiness gate needs that ordering).
    - Sources, the same the webhook uses today: **the matched ResourceFlavor's notes** (manufacturer/product/
      family; for an accelerated type memory/cores + the surviving `cpuDetail` note) **plus the Devices group's
      `AcceleratorSlicedDetail`** for the accelerator's `SlicedDetail`. **CPU-only / generic types have no
      Devices group** (R2): their Detail (CPU descriptor) comes entirely from the `cpuDetail` note, sourced from
      Node labels via `node_flavor.go:223-225` — which this spec keeps (only `acceleratorFeature` is removed).
      A Devices-only backfill would starve them.
    - **Readiness must admit a legitimately-empty Detail (R2).** For the collapsed generic pool the webhook
      today deliberately clears descriptors and sets `DisplayName = "CPU-only"` (`instance_type.go:150-158`) —
      "Detail populated" is undefined there, so a naive gate deadlocks the queue forever. Define readiness as
      "Detail *computed* for this type's kind" (accelerated → accelerator Detail incl. `SlicedDetail`; CPU-only →
      CPU Detail, which may be intentionally minimal), never "Detail non-empty".
    - **DisplayName default moves to the NodeFlavorReconciler's derived authoring, not the reconciler.** A
      derived InstanceType is stamped with `Spec.DisplayName` at creation — the flavor's product, or the
      `"CPU-only"` sentinel for the collapsed generic pool (the flavor carries an empty product there). This keeps
      the InstanceTypeReconciler status-only (no Spec write, no reconcile-loop dance). DisplayName is
      admin-editable, so a later rename is preserved; an admin-created type is not auto-named.
    - The readiness gate narrows the Kueue-admitted window but does **not** close the Pod-webhook window (T9):
      the entrance label is stamped by the webhook at admission and the Pod webhook fires on that label
      regardless of whether the queue object exists (`instance_type.go:141-143`, `pod.go:229-253`). The webhook
      empty-Detail path is a retryable rejection, handled in T9.
    - Acceptance: an accelerated derived type gains accelerator `Status.Detail` within one reconcile and it is
      **not erased on a second reconcile** (proves it lives in `computeStatus`); the `foldDetailCPU` note→Detail
      fold is unit-covered; a generic collapsed pool activates its queue with a minimal Detail (not deadlocked);
      and the NodeFlavorReconciler stamps the derived type's `DisplayName` at creation (flavor product, or
      `"CPU-only"` for the collapsed generic pool). Verify: `make generate` clean;
      `go test ./pkg/worker/controllers/worker/...`.

[x] **T9: re-plumb every Spec-descriptor reader to `Status.Detail`; drop webhook enrichment + the
    `acceleratorFeature` note producer (F6/F7).**
    - `pkg/worker/webhooks/worker/pod.go:253` — read per-card VRAM from `it.Status.Detail.Memory`. **Empty
      Detail is an explicit, retryable admission rejection** ("instance type not yet ready"), not a silent
      whole-card fallback or panic (the T8 gate does not make this unreachable). Bounded by one reconcile.
    - `pkg/worker/controllers/worker/instance.go:637,952-975` — `Spec.Manufacturer` → `Status.Detail.Manufacturer`.
    - `IsSliceable()` moves onto the Detail's `InstanceTypeAccelerator` and judges `SlicedDetail`. **The
      empty-Detail fail-safe must be a rejection, not a fall-through (R3, High).** Today an empty/false
      `IsSliceable` routes a *sliced* request into the whole-card branches (`webhooks/worker/instance.go:344-367,
      450-451`), so the Instance controller (`instance.go:947`) would later build a **sliced Pod with whole-card
      CPU/RAM** — silently wrong sizing, not a retry. So: the **Instance webhook must reject** a request that sets
      slice percentages while Detail is empty (retryable "not ready"), never default it whole-card; and the
      **Instance controller must requeue** (create no Pod) while `Status.Detail` is empty, so no Pod lands with a
      missing RuntimeClass (`instance.go:637`, whose empty path is silent) or bogus resource names
      (`:952-975`).
    - `InstanceTypeWebhook.Default` — delete the descriptor-enrichment block, which actually spans
      **`instance_type.go:145-252`** (not `:160-212`): the CPU-only clear + `"CPU-only"` DisplayName (`:150-158`;
      the DisplayName default now lives in the NodeFlavorReconciler's derived authoring, stamped at creation per
      T8), the flavor List + note fold incl. `Spec.Feature`/`acceleratorFeature` (`:160-212`), the
      DisplayName-from-Product default (`:214-223`), and `foldCPUDetail` (`:237-252`, which becomes dead code and
      must be removed or lint fails). Keep label/GeneralGroup/entrance stamping.
    - **Now remove the `acceleratorFeature` note producer** — its last reader is gone: delete the note write
      (`node_flavor.go:218-219`) and the `nodeFlavorAcceleratorsFeature` helper (`node_flavor.go:331-348`, which
      returns `workercore.AcceleratorsFeature` — leaving it would break T12's type deletion). The
      `cpuDetail`/manufacturer/product/family notes stay (they feed T8's Detail). Producer + last reader in one
      change fixes the T7/T9 ordering defect (Codex Critical).
    - Acceptance: sliced Pod admission reads VRAM from Detail (empty → retryable reject, asserted); a sliced
      Instance CREATE with empty Detail is **rejected**, not whole-card-defaulted (asserted); the Instance
      controller creates no Pod while Detail is empty; no descriptor written at admission; no `acceleratorFeature`
      string and no dead `foldCPUDetail` remain. Verify:
      `go test ./pkg/worker/webhooks/worker/... ./pkg/worker/controllers/worker/...`.

[x] **T10: remove the Spec descriptor fields; reorder + contiguously renumber; tighten immutability (F7).**
    - Delete `Manufacturer`/`Product`/`Family`/inline `InstanceTypeCPU`/inline `InstanceTypeAccelerator` from
      `InstanceTypeSpec`. **Reorder + contiguously renumber (2026-07-21 decision):** the reserve-and-guard
      approach is dropped spec-wide in favor of natural-number (gap-free) protobuf numbering. Reorder
      `InstanceTypeSpec` (admin-editable DisplayName/Description/Inactive first, then AcceleratorGroup/
      GeneralGroup/Acceleratable/OS/Arch/UnitResources/LocalStorage) and `InstanceTypeStatus` (Detail/Entrance/
      Phase/PhaseMessage/Accelerator/AcceleratorShared/AcceleratorSliced/CPU), renumbering each `protobuf` tag
      contiguously 1..N. **Delete `protobuf_reserved_test.go`** (the tag-audit guard is retired with the
      reservation approach). Add `Description` to the masked set in `validateInstanceTypeSpecImmutable`.
      `make generate`.
    - Acceptance: `InstanceTypeSpec` still comparable (compiles as a map key — the gateway
      `map[AggregatedInstanceTypeSpec]int` proves it); immutability freezes all but DisplayName/Description/Inactive
      (Description now editable); `generated.proto` numbers are contiguous 1..N; `make generate` clean. Verify:
      `go build ./...`, `go test ./pkg/worker/...`, `make lint`.

**Phase E — Gateway, cleanup, docs**  ·  *Final checkpoint: grep-clean of removed symbols; full suite + lint.*

[x] **T11: WorkerGateway aggregation (F8).**
    - Add `AcceleratorSlicedDetail` to the candidate and the tier; add `Detail` **only** to the status
      (`service/types.go`). A tier carries no identity — it groups candidates and holds a per-tier
      `AcceleratorSlicedDetail`; the observed descriptor lives solely on `Status.Detail`. `Recompute` aggregates
      by **direct summation** (profile Counts summed by name at candidate→tier→`Status.Detail.SlicedDetail`,
      Phase-independent).
    - **Maintain `Status.Detail` identity at ingestion (self-heal):** `adoptDetailIdentity` adopts a reconciled
      candidate's descriptor (`AcceleratorReady()`) at each ingestion site (`Next` + the `Handle` add/update
      paths); `Recompute` owns only `Detail.SlicedDetail`. A descriptor first seen empty (pre-reconcile window)
      self-heals once any candidate reports its hardware.
    - Re-plumb gateway readers of `AggregatedInstanceTypeSpec` Manufacturer/Product/Family to `Status.Detail`.
      Post-T10 the aggregated Spec no longer has those fields (build stays green ⇒ no in-tree reader survives),
      so this is a verify-none-remain step.
    - **Normalize `Description` (R9):** `normalizeAggregatedInstanceTypeSpec` (`helper.go:26-30`) zeroes only
      `Inactive`/`DisplayName` before the `map[AggregatedInstanceTypeSpec]int` collapse; `Description` is the
      same per-cluster admin annotation and must be zeroed too, or identical hardware splits into N aggregated
      items.
    - **v1 `InstanceTypeFlavorSpec.Sliceable` (field 8) removal (R10):** T7 dropped the extension-catalog
      `Sliceable`; the v1 API field (`api/worker/v1/instance_type_flavor.go:61`) is removed here — a second
      UI-visible shape change. It flows through `AggregatedInstanceTypeFlavorSpec`'s inline embed, so no separate
      aggregate field changes; renumber the trailing `GeneralGroup` (9→8) so the flavor spec stays contiguous,
      `make generate`, and clean the extensionapis `instance_type_flavor_test.go` `Sliceable` assertion.
    - Acceptance: `Recompute` tests sum same-name profile Counts across two clusters at tier + top; the
      `Status.Detail` identity self-heals from a not-yet-reconciled first candidate; two clusters with same
      hardware but different `Description` collapse to **one** aggregated item; a JSON fixture documents the new
      shape (`Detail` only on the item status). Verify: `go test ./pkg/workergateway/...`.

[x] **T12: delete the whole `AcceleratorsFeature` family + remove the R1 fallback (F1 cleanup).**
    - **Delete the whole `AcceleratorsFeature` family (2026-07-21 decision, R1 — within this spec, not deferred):**
      the `AcceleratorsFeature` / `AcceleratorSliced` structs, `MaxSlices()` / `SlicedCoresOvercommit()`,
      `MemoryPercentageStep`, and the `DevicesGroup.AcceleratorsFeature` field — renumbering the trailing
      `AcceleratorSlicedDetail` (12→11) so `DevicesGroup` stays contiguous; plus the `acceleratorFeature` note
      (operator-internal, gone in T9) and any genuinely-unused alias. Also close the pre-existing `Accelerator`
      field-5 gap (Status 6→5) for full contiguity; `InstanceTypeAccelerator` (orphaned once T10 dropped the
      inline embed) is deleted outright, so its field-4 gap is moot. `make generate`.
    - **Detector rewire (all 6):** each detector stashes the vendor soft-slice capability in the group
      `AcceleratorsFeature.LogicalSliced`, which T4 copies to each card's `LogicalSliced` (e.g.
      `nvidia/device.go:183` sets it, `:221-223` copies it). Set the per-card `LogicalSliced` (Count + overcommit)
      **directly** from that vendor computation, dropping the group intermediate; `SetGroupSlicedDetails` still
      aggregates the group `AcceleratorSlicedDetail` from per-card. Preserve each vendor's slicing condition
      (e.g. Ascend 910-only).
    - **Remove the R1 fallback:** drop the `AcceleratorsFeature.MaxSlices()` branch in `desiredSlicedCapacity`
      and the fallback fields in the Devices-watch signature (T5), and the device-plugin `server.go` fallback
      (T6). Convert the old-format test fixtures (`node_capacity` `devicesWithSlicing`, deviceplugin
      `twoCardDevices`) to per-card fixtures.
    - Acceptance: `grep` shows no `AcceleratorsFeature`/`MaxSlices`/`SlicedCoresOvercommit`/`acceleratorFeature`
      in non-generated, non-test Go; `DevicesGroup`/`Accelerator` protobuf numbers are contiguous (gap-free);
      `make generate` clean; `go build ./...`, full `go test ./...`, `make lint` all green. Verify: success
      criterion 4 + 5.

[x] **T13: MIG manual-lifecycle docs + stale-doc/spec annotations (F9 + R12).**
    - Add the `docs/` page (enable/disable/reboot-recovery/no-auto-descheduling) with the profile + placement
      tables and the grounding lifecycle facts; generic "a Kubernetes cluster" wording only.
    - Update the stale docs that describe the old group-level model: `docs/architecture.md:156,178` (group
      `AcceleratorsFeature`, per-vendor `maxSlices`, group-sized token pool) and `docs/walkthrough.md:394`
      (slice VRAM capping). Declare the supersession of the
      `2026-07-16-accelerator-slicing-capability-and-pool-feedback` spec's detector-`LogicalSliced` design in
      **this spec's header** (the "Supersedes prior design" block), leaving the archived spec untouched.
    - Acceptance: doc renders; sequences match the grounding facts; stale architecture/walkthrough passages
      updated; no host/cloud-specific names. Verify: manual read + grep for the old symbols in `docs/`.

**Checkpoints recap:** after T4 (data model live, unconsumed), after T7 (node/flavor migrated with the R1
dual-read fallback, non-MIG numeric identity proven incl. an old-format fixture), after T10 (InstanceType split
complete), after T12 (cleanup incl. full `AcceleratorsFeature` deletion + R1 fallback removal; full suite +
lint). Each leaves the tree buildable and tested.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
- **Factor the NVIDIA MIG profile logic into a pure function (T1).** The detector constructs NVML directly
  (`nvml.New(...)`, `device.go:40`) with no mock seam, and no per-vendor detector unit test exists today (only
  `pkg/devicemanager/detector/detector_test.go`). The profile derivation/filter must be a pure function over
  `[]GpuInstanceProfileInfo_v3` so it is table-testable without hardware; the NVML enumeration I/O stays on the
  hardware path (exercised only in spec 2 e2e).
- **Add per-vendor detector test files** where missing, so the T4 aggregation is covered per vendor.
- **Snapshot the pre-change `.sliced.*` capacity output** for a representative non-MIG fixture (NVIDIA multi-card
  group) before T5, to assert numeric identity after (success criterion 1) rather than eyeballing it.

#### Unit tests
Per-package coverage targets (percentages are targets to meet or exceed; concrete test names/coverage recorded
after the implementation PR merges):
- `binding/nvml`: `2026-07-20` - device-level profile accessor smoke (build-level; real call is hardware-only) - target `None` new % (thin cgo wrapper)
- `pkg/devicemanager/detector/nvidia`: `2026-07-20` - target `80%` (pure profile derivation/filter: A100 set → six-row table + H100 set proving `+me`/`+gfx` dropped with no name-collision; V1-without-Name geometry names; `MemorySlices` rounding; MIG vs non-MIG per-card keyed on current mode; group aggregation)
- `pkg/devicemanager/detector/{hygon,metax,cambricon,mthreads,ascend}`: `2026-07-20` - target `70%` (per-card `LogicalSliced` + group aggregation)
- `pkg/devicemanager/allocator`: `2026-07-20` - target: keep current % after `--slicing-policy` removal (no behavior change)
- `pkg/deviceplugin`: `2026-07-20` - target `75%` (`getListAndWatchResponse` per-card token count: non-MIG=`LogicalSliced.Count`, MIG=`PhysicalSliced.Count`, other modes unaffected)
- `pkg/worker/controllers/worker` (node_capacity): `2026-07-20` - target `80%` (non-MIG identity; mixed group units-vs-logical split; all-MIG; Devices-watch enqueue)
- `pkg/worker/controllers/worker` (instance_type): `2026-07-20` - target `80%` (Detail computed inside computeStatus + not stomped on re-reconcile; ResourceFlavor-note + Devices sourcing incl. CPU-only; per-type readiness incl. generic-pool empty Detail; `foldDetailCPU` unit-covered; derived DisplayName stamped in node_flavor, not the reconciler)
- `pkg/worker/webhooks/worker` (instance_type / pod / instance): `2026-07-20` - target `80%` (Default drops the `:145-252` enrichment + dead foldCPUDetail; immutability masks Description; Pod VRAM from Status.Detail with empty→retryable-reject; sliced Instance CREATE rejected on empty Detail, not whole-card-defaulted)
- `pkg/worker/extensionapis/worker`: `2026-07-20` - target: keep current % after `Sliceable` field removal
- `pkg/workergateway/service`: `2026-07-20` - target `80%` (Recompute direct-sum of profile Counts at tier + top; Manufacturer/etc. read from Detail)
- `api/worker/v1alpha1`: `2026-07-20` - `None` (generated deepcopy/protobuf; a comparability compile-check test for `InstanceTypeSpec` as a map key belongs in the consuming package)

#### Integration tests
Fake-client reconciler/webhook tests (the project's convention — no envtest cluster):
- **Non-MIG numeric identity (regression guard):** given a Node + Devices fixture with an all-logical NVIDIA
  group, the NodeCapacityReconciler patches the four `.sliced.*` keys to exactly the pre-change values.
- **Mixed group split:** a group with two logical 128-token cards + one MIG card → `.sliced.units` counts all
  three × D; the three logical keys count the two logical cards only; the device-plugin advertises
  `PhysicalSliced.Count` tokens for the MIG card and `LogicalSliced.Count` for the other two (assert the old
  behavior was 3×128 bare tokens, to document the truth-fix).
- **Old-format Devices (R1 rollout skew) — the load-bearing regression guard:** feed `desiredSlicedCapacity`
  and `getListAndWatchResponse` a Devices carrying only `AcceleratorsFeature` (no per-card /
  `AcceleratorSlicedDetail`); assert the four `.sliced.*` keys and the token pool are **preserved via the
  fallback**, not reverse-patched to null. Plus a mixed-fleet variant (node A new-format, node B old-format in
  one pool).
- **Non-Gi-aligned VRAM (R4):** a group whose `Memory` is not Gi-aligned (e.g. `43238` via the ECC-restore
  `×16/15`); assert `.sliced.memory-mib` and the Pod-webhook anchor use the identical lossy-label source as
  today (`42Gi` → 43008), not the exact `DevicesGroup.Memory`.
- **Node-vs-Devices skew:** feed `desiredSlicedCapacity` a Node whose `.count` label disagrees with the Devices
  group card count; assert the Devices ledger wins and the four-key output is the Devices-derived one.
- **Devices-driven convergence:** mutating only the Devices object (MIG toggled, simulating a DeviceManager
  restart re-detect) enqueues and reconciles the owning Node's capacity and the pool's InstanceType
  `Status.Detail`; assert Status/allocation-only churn does **not** enqueue (R7 predicate).
- **Two-group same-manufacturer node:** a node with one sliceable + one non-sliceable group of the same vendor
  (e.g. 910B + 310); assert per-group aggregation preserves non-MIG identity and the non-sliceable group
  contributes nothing.
- **Unhealthy card in group:** a card with `Status.Unhealthy=true` still fills `LogicalSliced`, so
  `sliceableCards == len(group.Accelerators)` and the identity holds.
- **T7→T9 ordering guard:** simulate the intermediate state (catalog `Sliceable` removed, note producer + its
  webhook reader still present); assert a new non-MIG InstanceType is still sliceable — proving the producer is
  not removed before its reader; and that T9 removes producer + enrichment (`:145-252`) + `foldCPUDetail`
  atomically so T10 compiles.
- **Empty-Detail admission is retryable / a rejection, never a mis-default (R3):** create an accelerated
  InstanceType; before its first reconcile, (a) a direct Pod on its entrance-label queue → Pod webhook returns
  a retryable "not ready" error (no whole-card fallback, no panic); (b) a **sliced Instance CREATE** with slice
  percentages set → webhook **rejects** (does not whole-card-default CPU/RAM); (c) the Instance controller
  creates **no Pod** while Detail is empty. After one reconcile, all succeed with slice-scaled sizing.
- **`computeStatus` does not stomp Detail (R7):** reconcile twice; assert the second pass keeps `Status.Detail`
  (proving Detail is computed inside `computeStatus`, not a separate `/status` write).
- **CPU-only Detail source + readiness (R2):** the `cpuDetail` note→Detail fold is unit-covered (`foldDetailCPU`,
  both the accelerated and CPU-only branches); a generic collapsed pool activates its queue with a minimal Detail
  (not deadlocked behind the readiness gate). The derived DisplayName (`"CPU-only"` for the collapsed pool) is
  asserted in the NodeFlavorReconciler derived-authoring test, not the reconciler.
- **MIG-mode matrix (current-keyed):** `current==ENABLE` (any pending) → physical only; `current==DISABLE`
  incl. pending-enable `(0,1)` → logical; `GetMigMode` not-supported/error → logical (a non-MIG-capable card
  keeps its soft slicing — this is the common V100/T4/RTX case, superseding the earlier R5 exclude-on-error).
- **Migration round-trip:** a stored InstanceType carrying the removed spec fields round-trips through a
  `/status` update (Detail backfilled, spec fields still present) and a subsequent main-resource update (fields
  pruned); reads work throughout because readers use `Status.Detail`.
- **Immutability + comparability:** ValidateUpdate rejects a frozen-field change (os/arch/groups/acceleratable/
  unitResources/localStorage) but allows DisplayName/Description/Inactive; `InstanceTypeSpec` used as a map key
  compiles and round-trips (the gateway `map[AggregatedInstanceTypeSpec]int` is the standing compile-check);
  `generated.proto` field numbers are contiguous 1..N.

#### e2e tests
- **Spec 1 (this spec): a non-MIG regression pass on the operator-e2e infra** (`testing/infra`) — bring up the
  chain on non-MIG accelerator nodes and assert the advertised `.sliced.*` capacities, the InstanceType
  `Status.Detail`, and admission of a soft-sliced workload are unchanged from before the spec. This is the
  observable proof of the "identical on non-MIG hardware" invariant.
- **MIG-path e2e is deferred to spec 2** (justification): there is no NVIDIA MIG hardware in this environment,
  and spec 1 lands no allocation path — a MIG card only changes advertised metadata, which the unit/integration
  layers cover. Real-card validation (A100 reset-required mode path; Hopper+ reset-free but non-persistent mode
  path) rides with the allocation spec that actually creates GI/CI.

## Alternatives

- **Anchor MIG requests on fractions (percentage/memory), auto-translating to profiles** — rejected. The
  mapping is many-to-one (`1g.10gb` vs `2g.10gb` share memory size), non-portable across generations (H100
  `1g.20gb` = 1/7 SM + 1/4 memory has no A100 analogue), a scalar cannot defend stranded capacity
  (placement/combination legality is two-dimensional), and it would need an annotation to signal MIG intent —
  against "the resource declaration is the intent". Profile-name anchoring matches NVIDIA `mixed`, the DRA
  driver, `mig-parted`, and KEP-4815's explicit-partition model. No fraction variant is kept.
- **Keep the flavor-note transcription for InstanceType enrichment** — rejected. Capability now changes at
  DeviceManager restart; an admission-time JSON-in-annotation snapshot is stale by design, occupies the wrong
  side of the desired/observed split, and admits corruption paths the typed status write does not.
- **Automate MIG mode via node config/labels (HAMi-style `operatingmode`)** — rejected. Mode switching is a
  heavyweight node operation (Ampere reset + client teardown); every automated dynamic-MIG geometry manager
  has retreated or stalled (Run:ai removed Dynamic MIG in v2.20; NVIDIA's DRA dynamic MIG is alpha,
  Hopper+-only, and demands exclusive ownership of node MIG state with intrusive cleanup).
- **Keep `MemoryPercentageStep` / group `MaxSlices()` as compatibility shims** — rejected. The step field is
  write-only today, and a group-level max would keep the per-card exclusion inexpressible — the exact defect
  this spec removes. Consumers get exact per-card/group data instead.
- **Enable `.sliced.mig-<profile>` capacity keys in this spec** — deferred to spec 2. A visible resource key
  with no admission/allocation path behind it invites requests that can only fail.

## Open Questions

All six drafting questions were resolved by the maintainer and folded into the sections above; recorded here so
the decisions are traceable:

1. **Overcommit aggregation semantics** — resolved. The two levels have distinct readers: the per-card
   `LogicalSliced` feeds AdmissionCheck's card decision; the group-level `AcceleratorSlicedDetail.Logical`
   feeds external queries ("does this node accept soft-slice requests, and does it permit compute
   overcommit"). `CoresPercentageOvercommit` is a per-model property, uniform within a group by construction,
   so aggregation takes the flag from any soft-sliceable card (false/meaningless when none). No OR/consistency
   ambiguity. (F1, F2)
2. **Flavor-catalog `Sliceable`** — resolved: removed from `InstanceTypeFlavorSpec` and its note parsing.
   Slicing detail is surfaced via `Status.Detail` / the gateway aggregate, not the flavor catalog. (F6)
3. **InstanceType write path** — resolved: all observed-hardware backfill detaches from the mutating webhook
   and moves to the InstanceTypeReconciler writing `Status.Detail` via the `/status` subresource (which does
   not pass through the admission webhooks, so no immutability conflict). The operator seeds Spec fields at
   Create; the DisplayName default (the flavor product, or `"CPU-only"` for the collapsed generic pool) is
   stamped once at derivation by the NodeFlavorReconciler, keeping the InstanceTypeReconciler status-only.
   DisplayName is admin-editable, so a later rename is preserved. (F7)
4. **Gateway tier `Detail` vs standalone `AcceleratorSlicedDetail`** — resolved: pure direct summation at every
   level; both the `Detail`-embedded `SlicedDetail` and the standalone field carry the identical Σ of profile
   counts. (F8)
5. **MIG cards in the bare `.sliced` token pool and `.sliced.units`** — resolved by the maintainer's F5
   correction: MIG cards are **not** dropped. The device-plugin `.sliced` pool re-sizes from the 128
   placeholder to the card's physical ceiling (`PhysicalSliced.Count`, new F1 field), and `.sliced.units` stays
   full-device (VRAM-anchored) counting MIG cards — preserving today's behavior. Only the three logical keys
   (`.sliced.cores/memory-percentage/memory-mib`) shed MIG cards, per the key-semantics split. `.sliced.mig-*`
   capacity-key timing is settled as spec 2. (F1, F4, F5)
6. **Node-reboot recovery for already-allocated Pods** — resolved: the administrator owns Pod lifecycle; the
   operator never auto-deschedules (documented in F9), and resetting a card's MIG mode already requires its
   instances idle, so a Pod must be stopped before a mode change regardless. After a reboot without a prior
   reset, DeviceManager re-detection realigns the Devices ledger to actual hardware; whether an already-held
   instance can then be recreated (ledger-alignment → recreate capability) is the follow-up allocation spec's
   job. (F9)
