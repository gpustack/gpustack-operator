# Spec: NVIDIA Dynamic MIG Allocation — Per-Card Profile Ledger, Incremental GI/CI Create/Destroy, Profile-Anchored Scheduling

Status: Building
Type: Feature

> **Second of two specs.** This is the follow-up "dynamic MIG implementation" spec that
> `2026-07-20-accelerator-slicing-metadata-realignment.md` (Shipped) named as its explicit Non-Goal. Spec 1
> restructured the metadata so the cluster records what is true about each card's slicing capability without
> changing any cluster-observable behavior; this spec builds the allocation path on top of that foundation and
> introduces the new schedulable `.sliced.mig-<profile>` resource. Nothing in spec 1 is a wrong design that
> needs compensating — its per-card `PhysicalSliced.Profiles` inventory, group `AcceleratorSlicedDetail`
> aggregate, real-NVML profile enumeration, and the "MIG card carries `LogicalSliced.Count == 0`" structural
> mutual exclusion are exactly the typed facts this spec consumes. The one deliberate deferral spec 1 made —
> that the bare `.sliced` token pool plus the scalar `.sliced.units` cannot express heterogeneous per-profile
> placeability — is resolved here by the per-card placement-aware `Remaining` ledger and the profile-anchored
> AdmissionCheck, precisely where spec 1 said that gating belonged.

## Summary
Turn the per-card MIG capability inventory spec 1 landed into a working allocation path. A currently
MIG-enabled card gains a per-card **profile ledger** in `Devices.Status` (`Allocated` = instances live and
bound, `Remaining` = instances still buildable given the card's current geometry and NVML placement slots),
computed on-node by the DeviceManager and consumed by the Kueue AdmissionCheck for placement-aware, per-card
profile filtering. Users request a hardware partition by **profile name** — `nvidia.com/gpu.sliced.mig-1g.10gb`
alongside the existing `nvidia.com/gpu.sliced` card-count key — mirroring NVIDIA's `mixed` strategy and the DRA
driver's attribute match; the Pod webhook validates the request and folds it into the existing `.sliced.units`
credit so Kueue quota accounting is unchanged. On the node, the device-plugin's `Allocate` creates the GI+CI
**incrementally** via direct NVML calls (reusing a free instance when one exists, else
`CreateGpuInstanceWithPlacement` at a computed non-overlapping slot), injects only `NVIDIA_VISIBLE_DEVICES=<MIG
UUID>` (hardware isolation, no `libvgpu.so`), and reclaims the instance on Pod exit with a bounded retry on
`NVML_ERROR_IN_USE`. The NVML operations are added as idiomatic Go wrappers in `binding/nvml/library_device.go`
over cgo symbols already emitted by `make generate nvml` — no new NVML library is introduced. MIG mode remains
a purely manual, administrator-driven node operation (documented in spec 1); this spec adds no mode automation.

## Motivation

Spec 1 made the cluster tell the truth about MIG capability but stopped short of letting anyone use it: a
MIG-enabled card reports its real profile inventory (`1g.5gb`×7, `2g.10gb`×3, …) and drops out of the three
logical soft-slice keys, yet there is no resource a workload can request to land on a MIG instance, no
admission gate that understands per-profile placeability, and no code path that creates or destroys a GPU
instance. The card is visible but inert.

The whole NVIDIA ecosystem **except HAMi** anchors a MIG request on the profile *name*: NVIDIA's
k8s-device-plugin `mixed` strategy registers `nvidia.com/mig-<profile>` per profile; the NVIDIA DRA driver
matches `device.attributes['gpu.nvidia.com'].profile == '1g.5gb'`; `mig-parted` configs and `nvidia-smi` speak
profile names. Anchoring on a fraction (memory/compute percentage) fails structurally — on A100-40GB `1g.10gb`
and `2g.10gb` have identical memory (2/8 slices) and differ only in compute, so a memory fraction cannot pick
between them; the same fraction lands on semantically different profiles across GPU generations (H100's
`1g.20gb` is 1/7 SM but 1/4 memory, a ratio no A100 profile has); and a scalar cannot express placement or
combination legality, so it cannot defend against stranded capacity. This spec therefore anchors physical
(MIG) requests on the profile name, keeping the three logical keys (`.sliced.cores-percentage` /
`.sliced.memory-percentage` / `.sliced.memory-mib`) for soft slicing exclusively — the key-semantics split spec
1 established.

The **feasible** form of dynamic MIG is "within-card, instance-level": once a card's MIG mode is on, GI/CI
create/destroy is online and per-instance — `nvmlGpuInstanceDestroy` / `nvmlComputeInstanceDestroy` return
`IN_USE` only when *that instance* has active processes, and workloads on sibling instances of the same card
are unaffected. What is a trap is "geometry-level" dynamism (whole-card re-slicing, defragmentation): it
requires an idle card and is eviction by another name; `mig-parted`'s `apply` is a destructive
"clear-the-whole-card then permutation-trial" operation, and every automated geometry manager has retreated
(Run:ai removed Dynamic MIG in v2.20; NVIDIA's DRA dynamic MIG is alpha, Hopper+-only, and demands exclusive
ownership of node MIG state with intrusive cleanup). This spec does only the feasible form: incremental
instance create/destroy on a card whose mode an administrator has already enabled, never touching geometry of
in-use cards and never automating the mode switch.

### Goals
- **A schedulable profile resource.** A workload requests `nvidia.com/gpu.sliced.mig-<profile>: 1` per card
  alongside `nvidia.com/gpu.sliced: <cards>`; the request is validated at ingress, gated by an admission check
  that understands per-card profile placeability, and actuated by the device-plugin creating the instance.
- **A per-card placement-aware `Remaining` ledger.** The DeviceManager publishes, per MIG card, how many instances
  of each profile are currently allocated and how many more can still be built given the occupied placement
  slots — folding the hardcoded NVML placement rules into a number the scheduler side consumes directly, which
  is the placement awareness `mig-parted` itself does not do.
- **Incremental GI/CI lifecycle via direct NVML.** Create a GI at a computed free placement slot and a CI
  spanning it, reusing a free sibling instance when one exists; destroy the CI then the GI on Pod exit with a
  bounded `IN_USE` retry. No shelling out to a `mig-parted` binary, no whole-card clear.
- **Complete `binding/nvml` for MIG lifecycle.** Add the missing idiomatic Go wrappers (`GpuInstance.Destroy`,
  `ComputeInstance.Destroy`, `Device.GetGpuInstancePossiblePlacements`, `Device.GetGpuInstances`,
  `GpuInstance.GetComputeInstanceProfileInfo`, `GpuInstance.GetComputeInstances`, a MIG-device UUID accessor)
  over the already-generated cgo symbols; no new NVML dependency.
- **Zero behavior change for non-MIG and soft-slice paths.** Exclusive / shared / soft-sliced allocation, Kueue
  quota semantics, and the four existing `.sliced.*` capacity keys are untouched; a node with no MIG-enabled
  card advertises no `.sliced.mig-*` keys and behaves identically to today.
- **Real-hardware validation on Hopper+ (H100).** GI/CI create/destroy, the `Remaining` ledger, and end-to-end
  admission of a `mig-<profile>` workload are validated on an H100 (reset-free mode enable; mode
  non-persistent across reboot). The A100 (Ampere, reset-required) mode path is documented but validated by the
  administrator, not in this environment.

**Success criteria (measurable):**
1. On an H100 with MIG mode enabled on a card, `Devices.Status` shows that card's per-profile `Allocated` and
   `Remaining` counts, and after a `mig-1g.10gb` workload is admitted the card hosts exactly one `1g.10gb` GI+CI,
   the workload container sees exactly that MIG device via `nvidia-smi`, and `Remaining[1g.10gb]` has decremented.
2. On Pod exit the instance is destroyed (CI then GI) and — absent a residual `IN_USE` process — `Remaining` returns
   to its pre-allocation value within one reclaim cycle; a destroy that races an `IN_USE` retries with bounds
   (surfacing an operator-visible condition at the bound) and never blocks allocation on sibling cards.
3. A `mig-<profile>` request whose profile is absent from the target pool's `AcceleratorSlicedDetail.Physical`
   is **rejected at the Pod/Instance webhook** with an actionable message; a request whose profile exists but
   whose cards are momentarily full/fragmented stays in **Retry** (never Reject) and admits once capacity frees.
   The existing AdmissionCheck never-Reject invariant is preserved.
4. On a node with **no** MIG-enabled card, no `.sliced.mig-*` capacity key is advertised, and the four existing
   `.sliced.*` values plus exclusive/shared/soft-sliced admission are numerically and behaviorally identical to
   before this spec.
5. A soft-slice (`.sliced.cores-percentage`/`.sliced.memory-percentage`/`.sliced.memory-mib`) request never
   lands on a MIG-enabled card (structural exclusion via `PhysicalSliced.Profiles` non-empty), and a
   `mig-<profile>` request never lands on a non-MIG card.
6. `make generate` is clean (CRDs, deepcopy, protobuf, applyconfigurations), `make generate nvml` re-emits the
   NVML bindings without manual edits, `go build ./...`, `go test ./...`, and `make lint` pass.

### Non-Goals
- **MIG mode automation.** No HAMi-style `nodeconfig`/label-triggered mode switching, no reconciler that flips
  MIG mode, no `nvmlDeviceSetMigMode` call on the hot path. Enabling/disabling MIG mode remains the manual,
  per-card-or-per-node administrator operation spec 1 documented (`nvidia-smi -i <id> -mig 1`, Ampere reset,
  DeviceManager restart to re-detect). Capability changes enter only via DeviceManager re-detection.
- **Geometry-level dynamism** — whole-card re-slicing, defragmentation, or any operation requiring an idle
  card. Rejected permanently, not deferred: re-slicing is eviction by another name. Fragmentation is relieved
  only as instances naturally release; the `Remaining` ledger keeps the scheduler from attempting an impossible
  placement.
- **A memory→profile translation layer.** A physical request names its profile explicitly. No fraction variant
  is offered; "users must know profile names" is an accepted cost, matching NVIDIA `mixed` and the DRA driver.
- **Mixing profiles within one container / MIG-on-MPS / cross-instance NCCL.** One profile per container per
  card (value exactly 1); no MPS layered on a MIG instance; MIG instances have no P2P/NCCL aggregation across
  the card. These are the same constraints NVIDIA's own `mixed` strategy carries.
- **Automatic descheduling around MIG changes.** GPUStack never evicts or deschedules a Pod in response to a
  MIG change; the administrator owns Pod lifecycle (and a card's mode cannot be reset while its instances are
  in use anyway).
- **Recovering a specific already-allocated Pod's instance across a node reboot.** MIG instances (and, on
  Hopper+, the mode itself) do not persist across reboot; the DeviceManager re-detects and realigns the ledger
  to actual post-reboot hardware, but reconstructing the exact instance a running Pod held is out of scope
  (the administrator redoes the enable sequence; controller-managed Pods recreate through normal scheduling).
- **A100 real-card e2e in this environment** (no A100 here); the Ampere reset-required path is documented for
  administrators and validated externally.
- **DRA / KEP-4815 migration.** How the ledger model might later map onto ResourceSlice counter sets is noted
  as a future question, not built here.

## Proposal

Add one new schedulable resource (`.sliced.mig-<profile>`) and the four pieces that make it work — a per-card
`Remaining`/`Allocated` profile ledger (published by the DeviceManager, consumed by the AdmissionCheck), an ingress
validation + units-folding step in the Pod webhook, a profile-aware AdmissionCheck feasibility branch, and an
incremental GI/CI create/destroy actuator in the device-plugin `Allocate`/reclaim path built on completed
`binding/nvml` wrappers — while leaving every non-MIG and soft-slice behavior byte-for-byte unchanged.

### Grounding facts (copied verbatim; sources are NVIDIA official docs/source and the in-tree code)

**Key-semantics split (from spec 1, restated).** `.sliced.cores-percentage` / `.sliced.memory-percentage` /
`.sliced.memory-mib` belong exclusively to logical (soft) slicing; `.sliced.mig-<profile>` belongs exclusively
to physical (MIG) slicing. A physical request names its profile explicitly. The two are mutually exclusive on
one container, and — by the per-card structural exclusion spec 1 landed — a MIG-enabled card
(`PhysicalSliced.Profiles` non-empty, `LogicalSliced.Count == 0`) never serves a soft slice and vice versa.

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

Profiles are **not portable across generations**: A100-80GB profile names double with memory
(`1g.10gb`/`2g.20gb`/…), and H100's `1g.20gb` is 1/7 SM but 1/4 memory. This is why the request anchors on the
profile name the card actually reports (enumerated by spec 1's NVML derivation), never on a computed fraction.

**Placement slots are hardcoded** (legal start:size in memory-slice units, from NVML placement data):
`1g.5gb` any of 0–6 (size 1); `1g.10gb` starts 0/2/4/6 (size 2); `2g.10gb` starts 0/2/4 only (size 2) — not 6,
because there is no 8th compute slice to pair with the 8th memory slice; `3g.20gb` starts 0 or 4 (size 4);
`4g.20gb` start 0 only (size 4); `7g.40gb` start 0 (size 8). Combination legality = the occupied slot
intervals do not overlap. Example: a card already holding `1×3g.20gb@slot0` can still build
`{1g.5gb:3, 1g.10gb:2, 2g.10gb:1, 3g.20gb:1}`. The DeviceManager enumerates these legal slots from NVML **once
at detect time** and caches them (F2 `Placements`); the reconciler then subtracts the occupied intervals it
reconstructs from Pod annotations to publish this per-card `Remaining` map (F3), so the scheduler side never
re-derives placement and no NVML runs per reconcile.

**Incremental GI/CI create/destroy — the NVML sequence** (from `mig-parted`'s `nvmlMigConfigManager`,
`pkg/mig/config/config.go`, adapted to be incremental and placement-aware instead of clear-and-permute).
`mig-parted`'s `SetMigConfig` creates GIs with **no** placement and, on any failure, clears the *entire* card
and permutation-retries — destructive whole-card semantics. This spec instead builds one instance at a chosen
free slot without disturbing siblings:

- *Create one instance of a profile:*
  1. `device.GetGpuInstanceProfileInfo(giProfileId)` → `giProfileInfo` (already wrapped, spec 1 T1).
  2. `device.GetGpuInstancePossiblePlacements(giProfileId)` → legal slots; drop any slot whose
     `[start, start+size)` overlaps an occupied interval; pick the lowest remaining (deterministic, packs low).
  3. `device.CreateGpuInstanceWithPlacement(&giProfileInfo, &placement)` → `gi` (already wrapped, spec 1 T1).
  4. `gi.GetComputeInstanceProfileInfo(ciProfileId, ciEngProfileId)` → `ciProfileInfo`. For the kept C==G
     profiles the CI spans the whole GI: `ciProfileId` is the slice-count-matched
     `NVML_COMPUTE_INSTANCE_PROFILE_*`, `ciEngProfileId` is `NVML_COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED`.
  5. `gi.CreateComputeInstance(&ciProfileInfo)` → `ci` (already wrapped, spec 1 T1).
  6. Resolve the MIG device UUID for `NVIDIA_VISIBLE_DEVICES` (`device.GetMigDeviceHandleByIndex` +
     the MIG-device UUID accessor).
- *Destroy one instance (reverse order):* walk the GI's compute instances, `ci.Destroy()` each, then
  `gi.Destroy()`. `Destroy` returns `IN_USE` only if that instance still has active processes — bounded retry.

**MIG lifecycle costs (from spec 1, restated).** *Mode switch (card-level, heavy):* Ampere (A100/A30) requires
a GPU reset after enabling and the mode persists across reboot via InfoROM; Hopper+ needs no reset but the mode
is **not** persistent across reboot. All daemons holding driver handles must stop first; a loaded `nvidia_drm`
makes the reset fail; passthrough VMs may forbid the reset (reboot the VM). Requires `CAP_SYS_ADMIN`. *Instance
management (GI/CI, light):* once mode is on, create/destroy is dynamic and online; instances never persist
across reboot.

**In-tree normalization (from the code).** The global denominator is `D = nodefeature.ResourceMaxUnits =
1_600_000` (also `CreditsPerCard`); each card's ledger seeds `Remaining = D`. The Pod webhook already folds a
soft slice's per-card budget into `.sliced.units` so Kueue and the device-plugin agree; a MIG request folds
`units = MemoryMibToUnits(profile.MemoryMib, cardVRAMMib)` — the **same VRAM-anchored fold** the soft
`.sliced.memory-mib` path already uses, so a MIG instance and a soft slice of the same VRAM charge the identical
credits (one credit scale, no hardcoded slice denominator, generation-agnostic; see the worked example below).
The Kueue `resourceTransformations` (`kuberess/apps_kueue.go`) maps `.sliced.units → credits` at factor 1
`multiplyBy .sliced` (per-card units × card count), so total credits = per-card units × cards; one whole card =
D credits and the ClusterQueue credits nominalQuota = `flavorCards × D`. The per-node `allocateMutex`
(added in spec 1's device-plugin fix) covers only the short identify→check→reserve; device mutation runs after
it is released. Because the NVIDIA path has no vendor serialization (MetaX/Cambricon carry a package `allocMu`),
this spec adds a **per-card lock** that guards MIG GI/CI create+destroy+marker-write and is the final arbiter of
the check-then-allocate race — sibling cards proceed in parallel (F4).

**Departure from the research sketch (a simplification this spec makes deliberately).** The research round
proposed the AdmissionCheck pass the chosen (card, profile) to the device-plugin via a Pod annotation. That
couples an operator-side decision to a fragile Workload→Pod annotation-propagation path. It is unnecessary: the
profile is already in the container's own `.sliced.mig-<profile>` request, which the device-plugin reads from
the Pod during `Allocate`; card+slot selection is the device-plugin's own concern under the per-card lock (F4).
The AdmissionCheck only gates *feasibility* (does a node with enough `Remaining[profile]` cards exist). No
*downward* steering annotation is introduced. What F3/F4 do add is the reverse, *upward* direction — the
device-plugin records the slot it chose into the Pod's **existing** allocation annotation (the same
`AllocatedAcceleratorAnnoKey` the scalar allocation already uses), which the reconciler reads to derive the
ledger (Decision 2). That is a plugin→Status record of a decision already made, not an operator→plugin
instruction, so it carries none of the rejected sketch's coupling. An on-disk ownership marker (F4) additionally
records the Pod→GI/CI binding for reclaim.

### User Stories

#### Story 1
As a **data-scientist / workload author**, I want to request a specific MIG profile by name —
`nvidia.com/gpu.sliced.mig-1g.10gb: 1` on two cards — through the same LocalQueue path I use for soft slices,
and have my Pod land on a real, hardware-isolated `1g.10gb` instance on each card, so that I get predictable
partitioned GPU without knowing anything about placement slots.

#### Story 2
As a **cluster administrator**, I want the DeviceManager to publish, per MIG-enabled card, exactly which
profiles are currently allocated and how many more of each can still be created given the card's live geometry,
so that the scheduler admits MIG workloads only where they can actually be placed and I can see stranded vs.
available capacity without running `nvidia-smi` on the node.

#### Story 3
As a **platform operator**, I want a MIG request whose profile the target pool does not offer to be rejected at
submission with a clear message (not to hang forever in a queue), while a request that is merely waiting for a
busy card to free stays pending and admits automatically once capacity returns, so that unsatisfiable requests
surface immediately and transient contention self-heals.

#### Story 4
As a **GPUStack maintainer**, I want the MIG instance lifecycle driven through our existing `binding/nvml` cgo
bindings (completing the missing wrappers) rather than by bundling and shelling out to a `mig-parted` binary,
so that instance create/destroy is a typed, in-process, transactional operation with the Devices ledger as its
boundary — and the container image carries no extra NVIDIA CLI pinned to a version with no stable library API.

### Core Features & Acceptance Criteria

#### F1 — Complete the `binding/nvml` MIG lifecycle wrappers + placement geometry (`binding/nvml/`)

The generated `nvml.go` already emits every cgo symbol this spec needs (`nvmlGpuInstanceDestroy`,
`nvmlComputeInstanceDestroy`, `nvmlDeviceGetGpuInstancePossiblePlacements`, `nvmlDeviceGetGpuInstances`,
`nvmlGpuInstanceGetComputeInstanceProfileInfo`, `nvmlGpuInstanceGetComputeInstances`,
`nvmlDeviceGetGpuInstanceRemainingCapacity`), and spec 1 T1 already added `GetGpuInstanceProfileInfo`,
`CreateGpuInstanceWithPlacement`, `CreateComputeInstance`, `CreateComputeInstanceWithPlacement`, and the
V1/V2/V3 profile handlers. Add the remaining idiomatic Go methods in the hand-maintained
`library_device.go` (which is **not** regenerated by c-for-go — only `nvml.go`/`const.go`/`zz_generated.types.go`
are), matching the existing accessor idiom (return a concrete type + `Return`, `C.GoString` for fixed C-string
fields):

- `func (l GpuInstance) Destroy() Return`
- `func (l ComputeInstance) Destroy() Return`
- `func (l Device) GetGpuInstancePossiblePlacements(profileId uint32) ([]GpuInstancePlacement, Return)` —
  two-call count-then-fill. NVML exposes a `_v2` variant (`nvmlDeviceGetGpuInstancePossiblePlacements_v2`)
  alongside the base symbol; prefer `_v2` with a base fallback, mirroring the V1/V2/V3 profile-info handlers.
- `func (l Device) GetGpuInstances(profileId uint32) ([]GpuInstance, Return)` — enumerate live GIs of a profile
  (to rebuild `Allocated` and to find a reusable free instance). **The occupied placement of each live GI comes
  from the already-wrapped `GpuInstance.GetInfo().Placement` (`{Start, Size}`)** — `GetGpuInstances` returns
  handles only, so F3 reads each handle's `GetInfo().Placement` to build the occupied intervals (a GI handle
  alone does not carry its slot).
- `func (l GpuInstance) GetComputeInstanceProfileInfo(ciProfileId, ciEngProfileId uint32) (ComputeInstanceProfileInfo, Return)`
- `func (l GpuInstance) GetComputeInstances(ciProfileId uint32) ([]ComputeInstance, Return)`
- A MIG-device UUID accessor sufficient to fill `NVIDIA_VISIBLE_DEVICES` from a `MigDevice` handle (the
  `Device.GetUUID` idiom already exists; extend it to the MIG-device handle the same way).
- Optionally `func (l Device) GetGpuInstanceRemainingCapacity(profileId uint32) (uint32, Return)` as a
  cross-check against the placement-derived free count (NVML returns this directly).

No new dependency, no `nvml.h`/`config.yaml` change (all symbols present); if a future symbol were missing,
the process is to confirm it in `nvml.h` and re-run `make generate nvml`. Note the API types here are the NVML
binding types (`GpuInstancePlacement`, `ComputeInstanceProfileInfo`) — distinct from the operator's
`AcceleratorPhysicalSlicedProfile` (F2/F5), which is the type carrying `MemorySlices`.

**MIG geometry splits by layer (Decision 1, refined for the platform link constraint).** The compute-slice-count
→ NVML GI/CI profile-id lookup (`MigProfileIDsForComputeSlices` + its maps) genuinely depends on NVML profile-id
constants, so it lands beside the wrappers in `binding/nvml` (a hand-maintained `mig_placement.go`) — consumed by
the detector's detect-time placement enumeration (F3) and the T7 allocator slot-pick. The **pure free-count math**
(`ComputeRemainingProfiles(occupied, possible)` + the half-open-interval overlap test) is *not* NVML-bound — it is
plain interval arithmetic on `{Start, Size}` — and its consumer is the reconciler (F3), which lives in
`pkg/deviceplugin`. That package transitively links Go's `plugin` package (via kueue/prometheus), forcing a
flat-namespace darwin test binary in which importing the cgo `binding/nvml` aborts at load on the unresolved NVML
symbols (verified: `dyld symbol not found in flat namespace '_nvmlComputeInstanceDestroy'`). So the free-count
math lives **operator-side** in `pkg/device` (a pure `mig_placement.go`, on the operator `AcceleratorPhysicalPlacement`
type), which the reconciler imports with zero cgo. **Hard layering rule (unchanged):** `binding/*` is the
foundational vendored cgo layer and must **not** import `api/worker` or `pkg/device`; anything producing operator
API types (`AcceleratorProfileCount`, `AcceleratorPhysicalPlacement`) stays above it (F2/F3). This *relocates* the
geometry the earlier T1 landed in `pkg/devicemanager/detector/nvidia`: the id maps move to `binding/nvml`, the
free-count moves to `pkg/device` (rewritten onto the operator interval type). It is a pure move; `make generate
nvml` still no-ops the generated files.

**Acceptance:** `make generate nvml` re-emits the generated files with no diff attributable to these wrappers
(all hand-maintained); the id-map table tests pass under `binding/nvml`, the `ComputeRemainingProfiles` table tests
under `pkg/device`; `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./...`, `go test ./binding/nvml/...`, and
`go test ./pkg/device/...` green (the real NVML calls are hardware-only, exercised in F8 e2e).

#### F2 — Per-card MIG profile ledger in `Devices.Status` (`api/worker/v1alpha1/devices.go`)

Extend `AcceleratorAllocation` (`Status.Groups[].Accelerators[]`) with a per-card physical ledger, reusing the
`{Name, Count}` shape spec 1 introduced for the capability aggregate so the type vocabulary stays consistent:

```go
// AcceleratorAllocatedProfile / free profile reuse the name+count pair. A dedicated
// status type (not the capability's AcceleratorSlicedPhysicalDetailProfile) keeps the
// allocation ledger and the capability inventory independently evolvable.
type AcceleratorProfileCount struct {
    Name  string `protobuf:"bytes,1,opt"`  // e.g. "1g.10gb"
    Count int32  `protobuf:"varint,2,opt"` // instances allocated / still buildable
}

// AcceleratorAllocation — existing type, extended. The scalar Allocated/Remaining
// units stay for exclusive/shared/soft-sliced accounting; the two profile maps are
// populated only for a MIG-enabled card and empty otherwise.
type AcceleratorAllocation struct {
    ID        string
    Index     uint32
    Mode      DeviceAllocationMode
    Allocated int32
    Remaining int32
    // AllocatedProfiles lists the MIG instances currently created and bound on this
    // card, by profile name. Empty for a non-MIG card.
    AllocatedProfiles []AcceleratorProfileCount
    // RemainingProfiles lists, by profile name, how many more instances of each profile can
    // still be created given the card's occupied placement slots. Empty for a non-MIG
    // card. This is the placement-aware number the AdmissionCheck gates on.
    RemainingProfiles []AcceleratorProfileCount
}
```

Protobuf numbers are assigned contiguously after the existing fields (no reservation, per the 2026-07-21
project decision — `AcceleratorAllocation` uses 1–5, so `AllocatedProfiles`/`RemainingProfiles` take 6 and 7;
`AcceleratorProfileCount` takes 1–2). `AcceleratorAllocation` is status-only (never a map key and never
`==`-compared — only `DeepEqual` paths touch it), so adding slice fields is safe.

**Placement transport for the annotation-merged ledger (Decision 2 — the mechanism is F3).** The ledger is
computed by the reconciler from Pod annotations plus a detect-time placement cache — **no live NVML in the
reconciler** — so two more shapes are added:

```go
// AcceleratorPhysicalPlacement is one memory-slice interval [Start, Start+Length).
type AcceleratorPhysicalPlacement struct {
    Start  int32 `protobuf:"varint,1,opt"`
    Length int32 `protobuf:"varint,2,opt"` // "Length" not "Size" — avoids the protobuf Size() method
}
```

- On the **capability** `AcceleratorPhysicalSlicedProfile` (`Devices.Spec`): add `Placements []AcceleratorPhysicalPlacement`
  (contiguous field 6) — the profile's **full empty-card legal-slot set**, enumerated once by the detector (F3,
  via `Device.GetGpuInstancePossiblePlacements` on a fresh card) and cached. The reconciler subtracts occupied
  from this cached set to derive `Remaining`, so caching the *full* set is correct whether or not NVML's
  possible-placements call is itself occupancy-aware. This type lives only under `Devices` (verified: **not** an
  `InstanceTypeSpec`/`FlavorSpec` map key, and its containers already hold slices), so the new slice is
  comparability-safe.
- On `AcceleratorAllocation` (the **annotation transport**): the device-plugin `Allocate` records the single MIG
  instance a Pod holds on a card as a scalar `AllocatedPhysicalProfile` (the profile name, contiguous field 8) plus
  `AllocatedPhysicalPlacements []AcceleratorPhysicalPlacement` (its occupied interval(s), contiguous field 9) — both carried
  in the Pod's `device.gpustack.ai/accelerator.allocated` annotation (a Pod holds one instance per card, but a
  slice keeps the merge uniform). The reconciler unions these across all the node's Pods into the per-card
  occupied set (`AllocatedProfiles`/`RemainingProfiles` counts are what it then writes to Status; these transport
  fields are annotation-side only, empty/omitted in the aggregated Status).

**Enrich the spec-1 group aggregate with the profile's memory (a required spec-1 refinement).** Spec 1's group
aggregate `AcceleratorSlicedPhysicalDetailProfile` carries only `{Name, Count}`, but F5's units fold needs the
profile's **`MemoryMib`**, and the Pod webhook reaches only the InstanceType (whose `Status.Detail.SlicedDetail`
is built from this aggregate) — it does **not** read Devices. Add `MemoryMib int64` (contiguous field 3) to
`AcceleratorSlicedPhysicalDetailProfile` so the aggregate is self-sufficient for the fold
`MemoryMibToUnits(MemoryMib, cardVRAMMib)`. **Deliberately not `MemorySlices` and not a `/Physical.Count`
denominator:** `Physical.Count` is the finest-profile max-instance ceiling (compute-limited — 7 on A100/H100),
which is **not** the memory-slice count (8), so a slice-count fold `MemorySlices/Count` over-charges the
whole-card profile on an asymmetric card (A100 `7g.40gb` = 8/7·D > a whole card); the two coincide only on
symmetric-slice cards. Folding on the reported VRAM (`MemoryMib/cardVRAM`) needs no per-card constant, is
generation-agnostic, and puts MIG on the **same** credit scale as the soft `.sliced.memory-mib` path — the
non-conflict property. `MemorySlices` stays a per-card field (`AcceleratorPhysicalSlicedProfile`) for placement
math (F3) only. This is the "complete spec 1 where spec 2 needs it" change the metadata realignment authorized;
the aggregate is not a map key, so the field is safe, and the detector's group-aggregation step (spec 1 F2)
carries `MemoryMib` through (uniform per profile name within a group).

**Acceptance:** `make generate` regenerates deepcopy/protobuf/applyconfigurations cleanly with contiguous
numbers; a non-MIG card serializes with empty profile maps + empty `AllocatedPhysicalPlacements` (omitempty), so
existing fixtures are byte-identical; the enriched aggregate round-trips `MemoryMib` from the detector to the
InstanceType Detail; the capability round-trips `Placements` from the detector into `Devices.Spec`.

#### F3 — DeviceManager publishes the ledger by annotation-merge + detect-time cache (`pkg/devicemanager/...`)

**The `DevicesReconciler` stays a pure annotation-merge; it does NO live NVML (Decision 2).** The reconciler is a
level-based controller that fires on every Pod add/delete/annotation-change on the node and rebuilds
`Devices.Status` wholesale from `Devices.Spec` + each Pod's allocation annotation. Injecting synchronous NVML I/O
(~50 ioctls per MIG card per reconcile, and NVML can block under driver contention or a concurrent `Allocate`'s
create/destroy) into that hot path would stall **every** mode's Status update on the node — an anti-pattern for a
reconciler that is otherwise pure K8s object arithmetic. So the MIG ledger is derived the same way the scalar
`Allocated`/`Remaining` already are: from the Pod annotations, folded **inside** the single wholesale Status
build (never a second write — the R7/dual-writer lesson still holds).

Two inputs feed the fold, both already present on the objects the reconciler reads:

- **`Placements` (the legal-slot cache)** — enumerated **once at detect time** by the DeviceManager
  (`Device.GetGpuInstancePossiblePlacements(profileId)` on the card) and stored in the capability
  `Devices.Spec…PhysicalSliced.Profiles[].Placements` (F2). It is static for a fixed MIG mode (re-enumerated only
  on re-detect), so it is read, not queried, per reconcile. Caching the **full empty-card** set means subtracting
  occupied gives the right `Remaining` regardless of whether NVML's call is occupancy-aware.
- **Occupied intervals** — the device-plugin `Allocate` (F4) records each MIG instance's chosen
  `{profile, start:size}` into the Pod's `device.gpustack.ai/accelerator.allocated` annotation
  (`AllocatedPhysicalPlacements`, F2), an *upward* record in the exact annotation the reconciler already reads (this is
  the reverse direction of the AdmissionCheck→plugin steering the spec rejects — see Proposal). The reconciler
  unions every live Pod's placements per card → the occupied set.

Then, per MIG-enabled card, folded into `applyAllocatedStatus`'s pass:
- **`Allocated`** = count the unioned placements by profile name.
- **`Remaining[profile]`** = `device.ComputeRemainingProfiles(occupied, Placements)` (the F1 pure geometry, in `pkg/device` —
  the reconciler must not import the cgo `binding/nvml`): count the cached legal slots whose
  `[start, start+size)` overlaps no occupied interval. Pure arithmetic, zero NVML.

**Accuracy trade-off vs the old live-NVML design (user-accepted).** Annotation-derived occupied only sees GIs
GPUStack allocated. It misses (a) instances an administrator created out of band and (b) crash-orphan GIs (created
but the annotation not yet written, or the Pod gone while the GI leaked). Those consume slots the ledger will not
count, so `Remaining` can **transiently overstate** → the AdmissionCheck admits → `Allocate`'s per-card lock, which
re-reads **live** NVML as the final arbiter (F4), fails → Pod recreate; the reclaim loop (F4/T8) GCs the orphan.
The churn is bounded and safe. This **replaces** the old design's "an orphan is reflected as occupied on the very
next pass" property: orphans are now caught at `Allocate` + reclaim, not in the reconciler. Crucially, the fold
can only **overstate** `Remaining` (missing occupancy → more free), never **understate** it (which would falsely strand
capacity): every placement the reconciler *does* see is subtracted, and the cached `Placements` is the full legal
set, so a seen instance never inflates `Remaining`.

- **Rollout skew (NEW):** a not-yet-upgraded DaemonSet writes no `Placements` (capability) and no
  `AllocatedPhysicalPlacements` (annotations), so a MIG card carries no `RemainingProfiles`. The AdmissionCheck (F6)
  distinguishes "no `RemainingProfiles`/`Placements` on any candidate card ⇒ ledger not ready" and Retries with an
  explicit "ledger not ready" message; document the upgrade ordering (roll the DaemonSet with or before the
  operator).

Because capability + `Placements` are enumerated at detect time and the ledger is recomputed every reconcile from
annotations, "the card is MIG-enabled" and "what Kueue-scheduled work sits on it" stay in sync with no NVML in the
reconciler and no mode state machine.

**Acceptance:** unit tests over the pure `ComputeRemainingProfiles(occupied, possible)` (in `pkg/device`, F1);
a reconciler-level (fake-client) test asserting: a MIG card's annotated placements fold to the right
`AllocatedProfiles`/`RemainingProfiles` (A100 `1×3g.20gb@slot0` → `Remaining{1g.5gb:3, 1g.10gb:2, 2g.10gb:1, 3g.20gb:1}`
against the cached `Placements`); a non-MIG card gets empty ledgers; the ledger **survives** a second reconcile
(recomputed inside the wholesale build, not stomped); occupied reconstructed from **two** same-profile Pods at
different slots reflects their real placements, not the empty-card ceiling; and `Remaining` never drops below the true
value for the placements seen (overstate-only). No NVML runs in these tests.

#### F4 — Incremental GI/CI create + reclaim in the device-plugin `Allocate` path (`pkg/deviceplugin/server.go`, allocator)

On `Allocate` for a `.sliced` request whose Pod also carries a `.sliced.mig-<profile>` request (read the
profile from the Pod's container resources; pod attribution via the existing `getAllocatingPod` +
`skipReserved` machinery spec 1 hardened). **Locking model (refined per cross-check):** the node-wide
`allocateMutex` today covers only identify→check→reserve and is released *before* the Responder does device
I/O; the NVIDIA path has **no** vendor serialization at all (MetaX/Cambricon carry a package-level `allocMu`).
This spec adds a **per-card lock** that guards MIG GI/CI create+destroy+marker-write, so concurrent creates on
the *same* card serialize their slot selection while sibling cards proceed in parallel; the node `allocateMutex`
keeps its short reserve role only.

1. Under `allocateMutex`, pick a card on this node that is MIG-enabled and has `Remaining[profile] ≥ 1` (the same
   ledger the AdmissionCheck used) and reserve it; release the node mutex.
2. Under the **per-card lock**, re-read the card's live state (final arbiter of the non-atomic reservation
   race): if a **free existing instance** of `profile` is present (an unbound GI — see the marker rule below),
   bind it; else create one incrementally (F1 sequence: possible-placements → pick lowest non-overlapping slot
   → `CreateGpuInstanceWithPlacement` → `GetComputeInstanceProfileInfo` → `CreateComputeInstance`).
3. **Persist ownership durably (NEW), in the same per-card critical section, before `Allocate` returns**: write
   an on-disk marker `podUID → {card, giId, ciId, migUUID, profile, start:size}` (atomic write, fail-closed
   parse), mirroring the MetaX/Cambricon `metax-sgpu.json`/`cambricon-smlu.json` pattern under the pods dir. The
   marker is what lets reclaim destroy the *exact* instance and lets step 2 tell a reusable-unbound GI from a
   bound one. It is written **inside** the create critical section so a crashed-then-retried `Allocate` (kubelet
   retries) rebinds its own GI rather than double-creating, and so the reclaim GC (below) cannot treat a
   just-created GI as an orphan mid-retry.
   **Also record the placement in the Pod annotation (Decision 2):** the Responder's `Allocate` result carries the
   chosen `{profile, start:size}` into the Pod's `device.gpustack.ai/accelerator.allocated` annotation
   (`AllocatedPhysicalPlacements`, F2) — the *upward* record the reconciler reads to reconstruct occupied and derive
   the ledger (F3). This is the same annotation the existing scalar allocation already uses; the MIG placement is
   an added field, patched by the same `patchAllocatingPod` path.
4. Inject **only** `NVIDIA_VISIBLE_DEVICES=<MIG UUID>` (hardware isolation) — do **not** inject `libvgpu.so` or
   the `CUDA_DEVICE_*_LIMIT` env the soft-slice path uses (the NVIDIA Responder gets a MIG branch). The NVML
   create/destroy calls this step and reclaim make live only on linux, so the actuator lives behind a
   `_linux.go`/`_other.go` build-tag seam (a darwin test binary links Go's `plugin` package, which forces a flat
   namespace where unresolved NVML symbols abort at load — the same reason MetaX/Cambricon split their vendor
   cgo). The pure slot-pick + marker logic stays platform-independent and seam-testable.
5. The ledger (`Allocated`/`Remaining`) is recomputed by the next `DevicesReconciler` pass from the **annotation this
   `Allocate` just wrote** (F3), not from live NVML and not written imperatively here.
6. **Transaction boundary**: if `CreateComputeInstance` fails, destroy the just-created GI (mirroring
   `mig-parted`'s cleanup); if the process dies between GI-create and marker/annotation-write, the GI is a
   **marker-less orphan** the annotation-merged ledger does **not** count, so `Remaining` transiently overstates until
   `Allocate`'s live per-card re-read (step 2) rejects a colliding placement and the orphan-GC (below) reclaims it
   when the card next drains — the user-accepted churn (F3). If the marker write or annotation patch fails, roll
   back (destroy the GI) so no half-owned instance persists.
7. **Reclaim** (separate worker, per-card locks — never the node mutex, so sibling cards are unblocked): reuse
   `RunSlicedReclaimLoop` + the `reclaimer` registry/`reclaimMaxMisses` debounce. On a Pod leaving the live set
   for `reclaimMaxMisses` consecutive passes, look up its marker and destroy the CI then the GI (F1 reverse
   sequence), bounded-retrying `NVML_ERROR_IN_USE` (treat like the vendors' partial-failure path — do not clear
   the miss counter, retry next pass; surface a condition at the bound). **Attribution self-check (NEW):** before
   destroying, reconcile the marker's `podUID` against the Pod's own allocation annotation, so a mis-attributed
   marker (the oldest-Pending `getAllocatingPod` heuristic can bind an Allocate to the wrong same-profile Pod)
   never destroys an instance a *running* Pod holds. Marker-less GIs are GC'd only when the card is fully
   drained (as MetaX does for unidentifiable orphans). Size the GC debounce (`reclaimMaxMisses` × resync) to
   exceed the kubelet Allocate-retry window.
8. `Remaining[profile] == 0` at step 2 → allocation fails (should not happen: the AdmissionCheck pre-gated; this is
   the safety net for the reservation race, surfacing as a normal device-plugin allocation error → Pod recreate).

**Acceptance:** the slot-pick, the reuse-vs-create decision, and the marker round-trip are pure/seam-testable
(fake NVML + a temp marker dir); a concurrent same-card Allocate test asserts no double-create/same-slot
collision under the per-card lock and that a sibling card is not blocked; a crash-then-retry test asserts the
retried Allocate rebinds its GI and the GC does not destroy it mid-retry; real GI/CI create/destroy +
`nvidia-smi` visibility + `libvgpu.so` absence are F8 e2e.

#### F5 — Pod webhook `mig-<profile>` validation + units folding (`pkg/worker/webhooks/worker/pod.go`)

Extend `slicedResourceNames` and `validateSlicedContainer` for the `.sliced.mig-<profile>` sub-key, and cover
`initContainers` as well as `Containers` (the current soft-slice validation scans `Containers` only — a
pre-existing gap this spec closes for the MIG keys, noted for the soft keys):

- **Validate (reject at ingress — Story 3 / criterion 3):** its value **must be exactly 1** (one instance per
  card; two instances ⇒ request two cards); **mutually exclusive** with the three logical keys — one container
  is either logical or physical; **at most one distinct `mig-<profile>` across the whole Pod** (all containers
  and initContainers) — a Pod naming two different profiles is **rejected**. This is not a stylistic limit: the
  device-plugin's `Allocate` attribution (`getAllocatingPod`, an oldest-Pending + limits-match heuristic) has no
  container identity and `parseCardRequest` (F6) models one profile per request, so a multi-profile Pod is
  *unattributable* at `Allocate` and mis-priced in quota — supporting it needs a per-container allocation
  protocol redesign, out of scope; rejecting at ingress is the honest failure (matching NVIDIA `mixed`
  practice). The profile name **must exist** in the target pool's `AcceleratorSlicedDetail.Physical.Profiles`,
  and the requested card count must not exceed the pool's static per-profile ceiling
  (`Detail.Physical.Profiles[name].Count` summed across cards). The pool is reached the **same way the webhook
  reads VRAM today — LocalQueue → InstanceType `Status.Detail` (`cardVRAMMib`, `pod.go:230-269`); the webhook
  does not read Devices**, so the profile inventory (with its `MemoryMib`) must be on the InstanceType Detail
  — which is why F2 enriches the group aggregate. A static violation is a **rejection**; the Instance webhook
  mirrors these checks and, on an empty `Status.Detail` (pre-reconcile window from spec 1), returns the same
  retryable "not ready" rejection spec 1 defined — never a whole-card default.
- **Default (units folding):** compute `units = MemoryMibToUnits(profile.MemoryMib, cardVRAMMib)` from the
  profile's `MemoryMib` (read off the InstanceType Detail's enriched aggregate, F2) — the **exact same fold**
  the soft `.sliced.memory-mib` path already uses — and fold it into `.sliced.units`, so Kueue ClusterQueue
  quota and `.sliced.units` accounting need **zero** change and a MIG instance charges identical credits to a
  soft slice of the same VRAM. Any client-supplied `.sliced.units` is ignored and recomputed.
- **`.sliced.units` for MIG is quota pricing ONLY, not a feasibility bound (semantics, per cross-check).** It is
  a loose VRAM-anchored charge for Kueue ClusterQueue accounting; feasibility/placeability is the `Remaining` ledger
  (F6), not units. There is **no hardcoded slice denominator** — the fold is `MemoryMib/cardVRAM`, generation-
  agnostic, so it is correct on asymmetric (A100/H100: 8 memory vs 7 compute slices) and symmetric cards alike;
  `Physical.Count` (the finest-profile max-instance ceiling) is deliberately **not** the fold denominator (it
  would over-charge the whole-card profile on an asymmetric card). Consequence to accept and document:
  compute-limited profiles under-charge (a card fully sliced into the finest profile does not consume all its
  VRAM — e.g. H100 `7×1g.10gb` ≈ 7/8·D; compute is unpriced, so same-memory profiles like `1g.20gb`/`2g.20gb`
  charge identically), so a ClusterQueue sized at `cards × D` can admit a workload on quota that then Retries on
  `Remaining==0` — designed-in head-of-line churn, an accepted quota-sizing property, not a silent bug (see the
  worked example after F8).

**Acceptance:** table tests — a valid `mig-1g.10gb: 1` on 2 cards folds `.sliced.units` to
`MemoryMibToUnits(10GiB, cardVRAM)` per card (identical to a soft `.sliced.memory-mib: 10Gi` request) and
passes; `mig-*: 2` rejected; `mig-*` + `.sliced.memory-mib` together rejected; a Pod with two containers naming
different profiles rejected; an unknown profile rejected with an actionable message; an `initContainer` `mig-*`
request is validated (not skipped); empty `Status.Detail` → retryable reject.

#### F6 — AdmissionCheck profile-aware feasibility, Retry-only (`pkg/worker/controllers/worker/node_devices_admission.go`)

Add a profile dimension to `cardRequest` and a MIG branch to `nodeDevicesFeasibility`, keeping the controller's
**never-Reject** invariant (all statically-unsatisfiable rejection happens at the F5 webhook; the AdmissionCheck
only ever returns Ready or Retry):

```
parseCardRequest: also read the .sliced.mig-<profile> sub-key → request.profile (the profile name), when
  present. Scan BOTH Containers and InitContainers (NEW): getAllocatingPod already scans InitContainers, and F5
  validates+folds them, so an init-only mig request must be visible to feasibility too — else it is folded and
  admitted but never Remaining-gated. F5 guarantees a single distinct profile per Pod, so request.profile is
  unambiguous.

nodeDevicesFeasibility (mig branch, request.profile != ""):
  candidate card = a card in the assigned pool's Devices where:
    1. PhysicalSliced.Profiles is non-empty            // MIG enabled (structural, from spec 1)
    2. Mode ∈ {None, Sliced}                            // not held whole-card by exclusive/shared
    3. Status RemainingProfiles[profile] ≥ 1                 // placement-aware, from F2/F3
  fit ≥ request.count → Ready ; else Retry (existing 30s backoff, never Reject)
  NB (readiness signal, Decision 2): a MIG card whose capability carries cached Placements has a ready ledger —
      empty RemainingProfiles then means "profile full" (Retry). Only when NO candidate MIG card has Placements cached
      (rollout skew: old DaemonSet) is the ledger "not ready" → Retry with a distinct explicit message. Keying
      readiness on Placements-cached (capability), not on RemainingProfiles-present (status), separates "full" from
      "not ready" (a full card also has empty RemainingProfiles).

logical-sliced branch: additionally EXCLUDE MIG-enabled cards (PhysicalSliced.Profiles non-empty) from the
  candidate set — the structural "an enabled MIG card offers no soft budget" gate at the admission layer.
```

The check-then-allocate window is not atomic (two Workloads can pass on the same `Remaining` snapshot); the
device-plugin per-card lock (F4 step 2) is the final arbiter, and a loser fails `Allocate` → Pod recreate. No
soft reservation state is added in this spec (kept simple; revisit only if the race proves material).

**Convergence is via Kueue's existing retry, not a new watch (per cross-check — this is liveness-correct).** A
freed slot is not stranded: the Retry deadline triggers Kueue's check-based eviction, the check resets to
Pending, the Workload requeues, and the re-reservation trips the operator's existing Workload watch, so
feasibility is re-evaluated. The cost to accept and document: each cycle is an **eviction + full requeue**
(≥30s latency per freed slot), and the retrying Workload **holds its quota reservation** through the backoff
(head-of-line blocking of that quota). The NodeCapacity Devices watch stays capability-only (spec 1's
`slicedDetailChanged`, correct — it feeds F7's static keys, not this gate); a status-ledger-filtered Devices
watch to speed this up is an **Ask-first** optimization, not a correctness requirement.

**Acceptance:** table tests over `nodeDevicesFeasibility` — a pool with `Remaining[1g.10gb]` on N cards admits a
request for ≤N and Retries for >N; a request for a profile with `Remaining == 0` everywhere (Placements cached)
Retries as "profile full" (does not Reject); a candidate set with no `Placements` cached on any MIG card →
"ledger not ready" Retry (distinct message); a MIG card is excluded from a logical-sliced candidate set; a card
`Mode == Exclusive` is not a MIG candidate even with `Remaining ≥ 1`; an init-container-only `mig-*` request is
counted by feasibility.

#### F7 — `.sliced.mig-<profile>` node capacity key + resource-name plumbing (`pkg/nodefeature`, `pkg/worker/controllers/worker/node_capacity.go`)

- Add `GetAcceleratableSlicedMigResourceName(manufacturer, profile string) core.ResourceName` in
  `pkg/nodefeature/knowns.go`, yielding `<base>.sliced.mig-<profile>` (e.g. `nvidia.com/gpu.sliced.mig-1g.10gb`),
  and register the suffix family so `IsKnownAcceleratableResourceName` and the Pod-webhook matcher recognize
  it.
- `NodeCapacityReconciler` (`desiredSlicedCapacity`, `:169-213`) advertises, per group with a non-empty
  `Detail.Physical.Profiles`, one `<base>.sliced.mig-<profile>` capacity key = `Detail.Physical.Profiles[name].Count`
  (static potential summed across the group's cards), merge-patched onto `Node.status.capacity` like the
  existing four `.sliced.*` keys — add the mig suffix family to `slicedCapacitySuffixes` (`:271-276`) so
  `isSlicedCapacityKey`/`buildSlicedCapacityPatch` reverse-patch it to null when the last MIG card leaves. These
  are Node extended resources, not device-plugin resources; they rely on kubelet mirroring `status.capacity →
  status.allocatable` for unmanaged extended resources — the **identical, shipped mechanism** the four existing
  `.sliced.*` keys already use (no device-plugin provider, no allocatable patch). This is verification, not new
  design; F8's e2e asserts scheduler-fit + kubelet admission + deletion accounting end to end.
- **NodeCapacity's Devices watch stays capability-only and that is correct** (cross-check refuted the earlier
  claim it was insufficient): these mig capacity keys are sourced from the **Spec-side** `AcceleratorSlicedDetail`
  (spec 1's `slicedDetailChanged` predicate, `node_capacity.go:441-471`), which the watch already fires on;
  extend the signature to include the profile geometry so a capability change re-enqueues. The **`Remaining`-ledger**
  (Status) drives the AdmissionCheck (F6), a separate concern with its own convergence — do not conflate.
- **Kueue ClusterQueue coverage (NEW — verify, match the soft keys).** The managed CQ's `buildResourceGroups`
  (`node_queue.go:260-289`) covers the **credits** resource (`GetAcceleratableCreditsResourceName`), not the
  individual `.sliced.*` keys; Kueue quota is charged on credits and the webhook's `.sliced.units` fold feeds
  that. `.sliced.mig-*` must get the **same** Kueue/quota treatment the soft `.sliced.*` keys get today (it
  piggybacks on credits; it is a Node-scheduler-fit + AdmissionCheck key, not a distinct CQ-covered quota key).
  Verify this holds during build and assert it in the F8 e2e — a mig workload must reach F5/F6, not strand in
  Kueue flavor assignment. If a build-time check shows the soft keys *are* individually CQ-covered, extend
  `buildResourceGroups` to cover the mig keys the same way.

**Acceptance:** table tests — a MIG group advertises `nvidia.com/gpu.sliced.mig-1g.5gb: 7` (etc.) equal to
`Detail.Physical.Profiles`, a non-MIG group advertises none, and toggling the last MIG card off reverse-patches
the keys to null; the four existing `.sliced.*` keys are unchanged on a non-MIG group (criterion 4); the
Kueue-coverage path is asserted end-to-end in F8.

#### F8 — Docs + real-card e2e (H100)

- Extend the spec-1 MIG operations doc (or add a companion page) with the **allocation** user contract: the
  request shape (`nvidia.com/gpu.sliced: <cards>` + `nvidia.com/gpu.sliced.mig-<profile>: 1`), the profile-name
  requirement (no fraction translation), the one-profile-per-container / value-must-be-1 rule, the
  MIG-must-go-through-Kueue usage limit, and the reclaim/`IN_USE` behavior. Copy the profile + placement tables
  in (no link to research working files).
- **H100 real-card e2e** (Hopper+, reset-free): enable MIG mode on a card, restart the DeviceManager, assert
  the `Remaining` ledger appears; submit a `mig-1g.10gb` LocalQueue workload, assert it admits, the container sees
  exactly the `1g.10gb` MIG device via `nvidia-smi`, `libvgpu.so` is absent, and `Remaining` decrements; delete the
  workload, assert the instance is destroyed and `Remaining` restores; note the mode is non-persistent across reboot.
- The **A100 (Ampere, reset-required)** mode path is documented for administrators; its reset-required enable
  sequence is validated externally, not in this environment.

**Acceptance:** doc renders with the tables and the request contract, generic "a Kubernetes cluster" wording;
the H100 e2e passes on the operator-e2e infra with a real card (deferred to the build/ship phase, run on the
H100 host).

#### Worked example — `.sliced.*` capacity & credits (H100 80GB, 4 cards)

The numbers implementation and tests check against. Constants: `D = ResourceMaxUnits = 1_600_000` (one whole
card = D credits); per-card VRAM = 80 GiB (`81920` MiB, lossy label); NVIDIA soft slicing
`LogicalSliced.Count = 128`, `CoresPercentageOvercommit = true`. Node-capacity formulas are spec 1 F4
(`.sliced.units`/cores/memory-\*) + F7 (`.sliced.mig-\*`); `sliceableCards` = cards with logical∨physical
capability, `softCards` = logical-only cards.

**Credits chain.** Kueue `resourceTransformations` (`kuberess/apps_kueue.go:436-448`) fold each mode into one
`credits` resource: exclusive `nvidia.com/gpu` → ×D, `.shared` → ×D/10, and **`.sliced.units` → ×1 `multiplyBy
.sliced`** (per-card units × card count). The ClusterQueue credits nominalQuota = `flavorCards × D`. Because the
Pod webhook folds **both** a soft slice and a MIG instance into `.sliced.units` via the same
`MemoryMibToUnits(memMib, cardVRAM)`, a MIG `3g.40gb` (40 GiB) and a soft `.sliced.memory-mib: 40Gi` request
charge the **identical** D/2 credits per card — one credit scale, which is the property that makes MIG and
logical non-conflicting.

Canonical H100-80GB MIG profiles (from NVML enumeration; `+me`/`+gfx` dropped) and their per-instance fold —
the per-card `Count` values come from the card's live enumeration at runtime, pinned here for the example:

| Profile | Memory | Max/card (`Count`) | units fold ≈ |
|---|---|---|---|
| 1g.10gb | 10 GiB | 7 | D/8 |
| 1g.20gb | 20 GiB | 4 | D/4 |
| 2g.20gb | 20 GiB | 3 | D/4 |
| 3g.40gb | 40 GiB | 2 | D/2 |
| 4g.40gb | 40 GiB | 1 | D/2 |
| 7g.80gb | 80 GiB | 1 | D   |

`PhysicalSliced.Count` (finest-profile max instances, sizes the bare `.sliced` token pool) = 7.

| key | A: all 4 logical | B: all 4 MIG | C: 1 MIG + 3 logical |
|---|---|---|---|
| sliceable / soft cards | 4 / 4 | 4 / 0 | 4 / 3 |
| `.sliced.units` | 4·D = 6,400,000 | 4·D = 6,400,000 | 4·D = 6,400,000 |
| `.sliced.cores-percentage` | (4·128)·100 = 51,200 | **not advertised (null)** | (3·128)·100 = 38,400 |
| `.sliced.memory-percentage` | 4·100 = 400 | **not advertised** | 3·100 = 300 |
| `.sliced.memory-mib` | 4·81920 = 327,680 | **not advertised** | 3·81920 = 245,760 |
| `.sliced.mig-<profile>` | none | 1g.10gb:28 / 1g.20gb:16 / 2g.20gb:12 / 3g.40gb:8 / 4g.40gb:4 / 7g.80gb:4 | 1g.10gb:7 / 1g.20gb:4 / 2g.20gb:3 / 3g.40gb:2 / 4g.40gb:1 / 7g.80gb:1 |
| CQ credits nominal | 4·D | 4·D | 4·D |

**Invariant across all three:** `.sliced.units` (node) = `sliceableCards·D` = CQ credits nominal =
`physicalCards·D` — a sliceable card always contributes exactly D whether soft or MIG, so MIG never inflates or
deflates the credit budget. The three logical keys scale by `softCards` (MIG cards shed, since a MIG card has
`LogicalSliced.Count == 0`); `.sliced.mig-*` scales by the MIG cards; both draw from the same shared `4·D`
credit pool via the identical `MemoryMibToUnits` fold, so a mixed node (C) shares its budget coherently with no
double-count. Scenario B illustrates C7: `mig-7g.80gb` costs D → 4 fit (= physical); `mig-1g.10gb` costs D/8 →
credits allow 32 but `Remaining` caps at 28, the 4 excess Retry (the accepted compute-limited under-charge).

### Notes / Constraints / Caveats

- **`binding/nvml` completion is additive hand-written wrappers**, not a regeneration: the cgo symbols already
  exist in the generated `nvml.go`; the new methods go in `library_device.go`. `make generate nvml` (c-for-go
  over `gen/binding/nvml`) must still produce a no-op diff for the generated files.
- **The reconciler must not import the cgo `binding/nvml`.** `pkg/deviceplugin` transitively links Go's `plugin`
  package (via kueue/prometheus), forcing a flat-namespace darwin test binary in which `binding/nvml`'s
  unresolved NVML symbols abort at load (verified). So the reconciler's placement-aware `Remaining` math is pure
  operator-side code in `pkg/device`; only genuinely NVML-constant-bound geometry (profile-id maps) lives in
  `binding/nvml`, consumed by the detector (detect-cache) and the T7 allocator, which do not link `plugin`.
- **`AcceleratorAllocation` is status-only** — the new slice fields are safe there; do not let any profile-slice
  type leak into a map-key type (`InstanceTypeSpec`/`InstanceTypeFlavorSpec`), per spec 1's comparability
  constraint.
- **Behavior invariance** on non-MIG and soft-slice paths is a review gate for every touched site: same
  inputs, same advertised numbers, same admission verdicts.
- **`make generate` from the main checkout** after any `api/` or webhook change (go-to-protobuf needs the cwd
  to end in `gpustack.ai/gpustack`); `make lint` has a cold-cache timeout — use a long timeout or background it.
- **Profile names are the card's own NVML-reported names** (spec 1's derivation), so the resource key, the
  ledger, and the webhook validation all speak the same vocabulary; no separate profile table is hardcoded in
  this spec.
- **The reservation race is handled by the per-node `allocateMutex` (short reserve) plus a new per-card lock
  (device mutation)**, not a new distributed reservation; this is a conscious simplicity choice, revisited only
  if real-hardware contention proves it insufficient.

### Boundaries

- **Always:** drive MIG instance lifecycle through `binding/nvml` (no bundled `mig-parted` binary); create GIs
  with an explicit non-overlapping placement (never clear-and-permute the whole card); inject only the MIG UUID
  for a MIG allocation (no `libvgpu.so`); preserve exclusive/shared/soft-sliced behavior and the four existing
  `.sliced.*` values byte-for-byte on non-MIG nodes; run `make generate` / `make generate nvml` from the main
  checkout; sign off every commit (`--signoff`); keep docs cloud-provider-generic.
- **Ask first:** adding any MIG *mode* management (even assisted) — the design is manual-only; adding a soft
  reservation/TTL layer to the AdmissionCheck; changing Kueue quota math or the `.sliced.units` folding
  constant; any change to the gateway REST shape.
- **Never:** automate MIG mode switching or call `nvmlDeviceSetMigMode` on the allocation path; re-slice or
  defragment an in-use card; add a memory→profile translation; introduce a new NVML library or vendor a
  `mig-parted` binary; make the AdmissionCheck Reject (rejection lives at the ingress webhook only); reference
  research working files from code or docs.

### Risks and Mitigations

- **Non-atomic check-then-allocate race** → two Workloads pass the AdmissionCheck on one `Remaining` snapshot and
  collide at `Allocate`. Mitigation: the per-node `allocateMutex` re-checks `Remaining[profile]` live and is the
  sole arbiter; the loser fails allocation and recreates through normal scheduling; the ledger is recomputed
  after every create so the next snapshot is fresh. Covered by a concurrent-Allocate test on a fake seam.
- **`Destroy` returns `IN_USE` from a residual process** → a straggler on the instance blocks reclaim.
  Mitigation: bounded retry with backoff on that instance only, never blocking sibling-card allocation; surface
  a condition/metric after the bound; reuse the MetaX/Cambricon reclaim-loop debounce so retries don't storm.
- **Ledger patch amplification** → recomputing `Devices.Status` on every reconcile could churn writes.
  Mitigation: the ledger rides the reconciler's existing `DeepEqual`-gated Status update — it is recomputed
  in-memory each pass but written only when it actually changed (the same gate the scalar allocation already
  uses); no separate publish, no NVML, so there is no extra write path to amplify.
- **`.sliced.mig-*` as a Node extended resource without a device-plugin provider** → kubelet must admit a Pod
  requesting it. Mitigation: these keys follow the exact merge-patch-onto-Node-capacity model the four existing
  `.sliced.*` keys already use (scheduler-accounted integer resources, not device-plugin resources); the bare
  `.sliced` device-plugin pool does the injection. A non-MIG node advertises none, so nothing changes there.
- **CI profile / engine-profile selection wrong for a kept profile** → a mismatched CI profile fails
  `CreateComputeInstance`. Mitigation: for the C==G profiles this spec keeps, the CI spans the whole GI
  (slice-count-matched profile, `SHARED` engine profile); validate the create against `mig-parted`'s observed
  sequence and cover with the H100 e2e; a create failure is caught and surfaced, never leaves a half-built GI
  (destroy the GI on CI-create failure, mirroring `mig-parted`'s cleanup).
- **Profile-name drift across drivers/generations** → a hardcoded table would rot. Mitigation: the spec never
  hardcodes profile names; it uses the card's NVML-reported names (spec 1's derivation) end to end.
- **Bypassing Kueue** → a Pod requesting `.sliced.mig-*` outside a LocalQueue is not placement-gated.
  Mitigation: document "MIG requests must go through Kueue/LocalQueue" as a usage limit (same risk surface as
  the existing soft keys); the device-plugin `Allocate` still fails safe (no free slot ⇒ allocation error)
  rather than corrupting the card.
- **Dual-writer stomp on `Devices.Status`** → `DevicesReconciler.Reconcile` rebuilds Status wholesale each pass;
  any second, out-of-band ledger write would be erased on the next reconcile (spec 1's R7 lesson). Mitigation:
  the MIG ledger is derived **inside** that single Status computation from Pod annotations + the cached
  `Placements` (F3), exactly like the scalar allocation merge — no NVML, no second write; a test asserts the
  ledger survives a second reconcile.
- **Reconciler device-I/O anti-pattern (Decision 2)** → doing live NVML per reconcile (an earlier design) put
  ~50 blockable ioctls per MIG card into a hot, frequently-firing, level-based reconciler, stalling all modes'
  Status updates. Mitigation: the reconciler is pure annotation-merge + arithmetic; NVML lives only at detect
  time (cache `Placements` once) and on the `Allocate`/reclaim path (F4), never in the reconciler.
- **Annotation-derived `Remaining` overstates (accepted)** → occupied is reconstructed only from Pod annotations, so
  out-of-band or crash-orphan GIs are uncounted and `Remaining` can transiently overstate → admit-then-fail churn.
  Mitigation: `Remaining` can only **overstate** (never understate → never falsely strand capacity); `Allocate`'s
  per-card lock re-reads live NVML as the final arbiter and reclaim GCs orphans; a test asserts the overstate-only
  direction. User-accepted, bounded churn (F3).
- **Reuse-vs-orphan-GC race** → a GI left by a crashed-then-retried `Allocate` is simultaneously "reusable"
  (F4 step 2) and "collectable orphan" (F4 step 7). Mitigation: write the ownership marker in the same per-card
  critical section as the GI create, before `Allocate` returns, and size the GC debounce (`reclaimMaxMisses` ×
  resync) to exceed the kubelet Allocate-retry window; a crash-then-retry test guards it.
- **Attribution misbinding destroys a running Pod's instance** → `getAllocatingPod`'s oldest-Pending heuristic
  can bind an Allocate (and its marker) to the wrong same-profile Pod; when that wrong Pod exits, reclaim would
  destroy a live instance. Mitigation: reclaim self-checks the marker `podUID` against the Pod's allocation
  annotation before destroy (F4 step 7); a reordered-admission test asserts no live instance is reclaimed.
- **MIG key strands in Kueue flavor assignment** → if the managed ClusterQueue does not treat `.sliced.mig-*`
  the way it treats the soft keys, the workload sits Pending with no F5/F6 verdict (permanent, silent).
  Mitigation: F7 verifies MIG keys get the same credits-based Kueue treatment as the soft `.sliced.*` keys and
  asserts a mig workload reaches admission in the F8 e2e; extend `buildResourceGroups` only if the soft keys are
  found to be individually CQ-covered.
- **Rollout skew (operator ahead of DaemonSet)** → a spec-1-format DaemonSet caches no `Placements` and writes no
  MIG annotation, so a mig workload would Retry forever with a misleading verdict while F5 (Spec-side) passes and
  F7 advertises the keys. Mitigation: F6 distinguishes "ledger not ready" (no `Placements` cached on any candidate
  MIG card) with an explicit Retry message; document the upgrade ordering (roll the DaemonSet with or before the
  operator). Transient, self-heals once the DaemonSet rolls.
- **Retry convergence cost (head-of-line)** → the AdmissionCheck converges a freed slot via Kueue's
  eviction+full-requeue (≥30s per cycle), and the retrying Workload holds its quota reservation through the
  backoff. Mitigation: accepted and documented as the convergence cost; a status-ledger Devices watch to
  shorten it is an Ask-first optimization, not built here.

## Design Details

### Commands

**Environment: local `darwin`** — the whole module including the CGO `binding/nvml` wrappers builds and
unit-tests locally; no Linux host or NVIDIA SDK is needed for Go verification (only the container image build
and the real-card e2e need hardware). There is **no MIG hardware here**, so NVML enumeration/create/destroy
cannot run locally; the placement/slot logic, the units folding, the feasibility branch, and the ledger
derivation are factored into pure functions verified by table tests, and real GI/CI lifecycle is the H100 e2e.

```bash
# Regenerate the NVML cgo bindings (c-for-go over gen/binding/nvml) — expect a no-op diff for the
# generated files since the wrappers live in the hand-maintained library_device.go:
make generate nvml

# Regenerate CRDs/deepcopy/protobuf/applyconfigurations after api/ or webhook changes.
# MUST run from the main checkout (go-to-protobuf needs cwd ending in gpustack.ai/gpustack):
make generate

# Build & test:
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./...
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test ./...
# Fast inner loop, e.g.:
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test ./pkg/worker/webhooks/worker/... \
  ./pkg/worker/controllers/worker/... ./pkg/devicemanager/... ./binding/nvml/...

# Lint (cold cache blows a 2-min timeout, ~20s warm — long timeout or background):
make lint
```

### Project Structure (touched surface)

```
binding/nvml/library_device.go            # F1: Destroy / PossiblePlacements / GetGpuInstances /
                                          #     GetComputeInstanceProfileInfo / GetComputeInstances / MIG UUID
binding/nvml/mig_placement.go             # F1: NVML-native geometry — profile-id maps (+ T7 slot-pick overlap)
pkg/device/physical_placement.go               # F1/F3: pure free-count geometry (ComputeRemainingProfiles) on operator types
api/worker/v1alpha1/devices.go            # F2: AcceleratorProfileCount + ledger fields; AcceleratorPhysicalPlacement;
                                          #     capability Placements; annotation AllocatedPhysicalPlacements
pkg/device/types.go                       # F2: aliases for the new status/placement types
pkg/devicemanager/detector/nvidia/...     # F3: detect-time enumerate + cache Placements into Devices.Spec
pkg/deviceplugin/controller.go            # F3: reconciler annotation-merge → Allocated/Remaining (no NVML)
pkg/deviceplugin/server.go                # F4: mig branch in Allocate; reclaim; MIG UUID injection; annotation write
pkg/devicemanager/allocator/nvidia/...    # F4: incremental GI/CI create/destroy actuator (_linux/_other seam)
pkg/worker/webhooks/worker/pod.go         # F5: mig-<profile> validation + units folding; initContainers
pkg/worker/webhooks/worker/instance.go    # F5: mirror validation; empty-Detail retryable reject
pkg/worker/controllers/worker/node_devices_admission.go  # F6: profile dimension + mig feasibility branch
pkg/nodefeature/knowns.go                 # F7: GetAcceleratableSlicedMigResourceName + suffix registration
pkg/worker/controllers/worker/node_capacity.go           # F7: advertise .sliced.mig-<profile> capacity keys
docs/                                     # F8: allocation user contract + profile/placement tables
testing/infra                             # F8: H100 real-card e2e case
```

### Code Style

Binding wrappers follow the existing `library_device.go` idiom — concrete return + `Return`, `C.GoString` for
fixed C-string fields (no hand-rolled byte loops). Existing accessors pre-size from known bounds; the list
calls added here introduce a two-call count-then-fill (query count, then fill), and the possible-placements
wrapper prefers the `_v2` symbol with a base fallback, like the V1/V2/V3 profile handlers spec 1 added:

```go
// GetGpuInstancePossiblePlacements returns the legal placements for the given GPU
// instance profile on this device. It queries the count first, then fills the slice.
func (l Device) GetGpuInstancePossiblePlacements(profileId uint32) ([]GpuInstancePlacement, Return) {
    var count uint32
    if ret := nvmlDeviceGetGpuInstancePossiblePlacements(l.nvmlDevice, profileId, nil, &count); ret != SUCCESS {
        return nil, ret
    }
    if count == 0 {
        return nil, SUCCESS
    }
    placements := make([]GpuInstancePlacement, count)
    ret := nvmlDeviceGetGpuInstancePossiblePlacements(l.nvmlDevice, profileId, &placements[0], &count)
    return placements[:count], ret
}
```

Multi-word Go files in snake_case; table-driven tests with a shared execution loop; fake clients/seams; assert
observable state (advertised capacities, ledger content, admission verdict, injected env), not implementation
details.

### Implementation Plan

**Dependency graph & strategy.** F1 (binding wrappers) and F2 (ledger types + resource-name plumbing) are
additive foundations with no consumers, so they land first. F3 (publish the ledger) needs F1+F2. The admission
path (F5 validate/fold, F6 gate) needs F2's enriched aggregate and F3's `Remaining`. The actuator (F4 create, then
reclaim) needs F1+F3 and is the riskiest, hardware-only-verifiable piece. The user-visible capacity key (F7)
lands **after** the actuator works, so we never advertise a schedulable `.sliced.mig-*` whose data path isn't
ready. The tree builds and tests green at every checkpoint. The de-risk-first task is the pure placement/free
math (T1), since there is no local MIG hardware and it is the novel logic; the CI-profile mapping is pinned
there too (Open Question 1).

**Phase A — De-risk & foundations**

[x] **T1 (PoC / de-risk): pure placement-free math + profile↔NVML-id mapping + units fold.**
    - Add (no NVML I/O): `ComputeRemainingProfiles(occupied []Placement, possible map[profileName][]Placement)
      map[profileName]int` (count non-overlapping legal slots per profile); the `profileName ↔ {GI profile id,
      CI profile id, CI eng profile id}` table for the kept C==G profiles (pin against `mig-parted`'s
      `SetMigConfig` sequence + NVML `const.go` ids — Open Question 1). The units fold **reuses the existing
      `nodefeature.MemoryMibToUnits`** (no new fold function, no `/8` constant).
    - Acceptance: table tests — A100 worked example (`1×3g.20gb@slot0` → `{1g.5gb:3, 1g.10gb:2, 2g.10gb:1,
      3g.20gb:1}`), empty card → per-profile ceilings, a fully-fragmented card → the correct reduced map; the
      CI mapping covers `1g.5gb/1g.10gb/2g.10gb/3g.20gb/4g.20gb/7g.40gb`; the fold equals
      `MemoryMibToUnits(profile.MemoryMib, cardVRAM)` (identical to a same-VRAM soft request).
      Verify: `go test ./pkg/devicemanager/...`.
    - **Re-plan (Decision 1, refined in T4):** this geometry landed in `pkg/devicemanager/detector/nvidia`. T4
      splits it by layer — the NVML-constant-bound id maps to `binding/nvml`, the pure interval free-count to
      `pkg/device` (the reconciler cannot import the cgo `binding/nvml`; see F1/T4).

[x] **T2: F1 — complete the `binding/nvml` MIG wrappers.**
    - Add the wrappers listed in F1 to `library_device.go` (Destroy×2, `GetGpuInstancePossiblePlacements` with
      `_v2`-preferred fallback, `GetGpuInstances`, `GetComputeInstanceProfileInfo`, `GetComputeInstances`, the
      MIG-device UUID accessor; confirm `GpuInstance.GetInfo().Placement` exposes `{Start,Size}`).
    - Acceptance: `make generate nvml` produces a no-op diff for the generated files (wrappers live in the hand
      file); `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./...` and `go test ./binding/nvml/...` green.

[x] **T3: F2 — ledger API types + aggregate memory enrichment + resource-name plumbing.**
    - `api/worker/v1alpha1/devices.go`: add `AcceleratorProfileCount{Name,Count}`, `AllocatedProfiles`/
      `RemainingProfiles` on `AcceleratorAllocation` (contiguous fields 6/7), and `MemoryMib` (field 3) on the
      group aggregate `AcceleratorSlicedPhysicalDetailProfile` (the fold input; `MemorySlices` stays a per-card
      field for placement); `pkg/device/types.go` alias if needed.
    - `pkg/nodefeature/knowns.go`: `GetAcceleratableSlicedMigResourceName(manufacturer, profile)` +
      register the `.sliced.mig-<profile>` suffix so `IsKnownAcceleratableResourceName` and the Pod-webhook
      matcher recognize it.
    - Detector (spec 1 F2 aggregation) carries `MemoryMib` through into the group aggregate + InstanceType
      Detail.
    - Acceptance: `make generate` clean, contiguous protobuf numbers; non-MIG card serializes byte-identically;
      `MemoryMib` round-trips detector → aggregate → InstanceType Detail. Verify: `go build ./...`.

> **Checkpoint A:** foundations in place, no consumer yet; full build + `go test ./...` green.

**Phase B — Publish the ledger by annotation-merge + detect cache (nothing allocates yet)**

[x] **T4: F1(geometry)+F2(placement fields)+F3 — reconciler annotation-merge ledger, no NVML hook.**
    This is the re-planned T4 (Decisions 1 & 2); it supersedes the earlier NVML-hook build (drop the
    `ProfileLedgerFunc`/`RegisterProfileLedger` hook, the `detector.ProfileLedger` NVML read, and the allocator
    `_linux/_other` ledger seam). Steps:
    - **Relocate the T1 geometry by layer** (Decision 1, refined): the profile-id maps
      (`MigProfileIDsForComputeSlices` + its maps) → `binding/nvml/mig_placement.go`; the pure free-count
      (`ComputeRemainingProfiles` + overlap) → `pkg/device/physical_placement.go` on the operator `AcceleratorPhysicalPlacement`
      type. The reconciler (`pkg/deviceplugin`) links Go's `plugin` package and cannot import the cgo
      `binding/nvml` (verified darwin load abort), so the free-count it consumes stays operator-side. Move the
      table tests with each; delete the detector's `mig_placement.go`; update importers.
    - **API additions (F2):** `AcceleratorPhysicalPlacement{Start,Length}` (Length not Size — avoids the protobuf
      `Size()` collision); capability `AcceleratorPhysicalSlicedProfile.Placements []AcceleratorPhysicalPlacement`
      (field 6); annotation-transport `AcceleratorAllocation.AllocatedPhysicalProfile` (field 8) +
      `AllocatedPhysicalPlacements` (field 9). `make generate` (from the main checkout), contiguous numbers.
    - **Detect-time cache (F3):** the NVIDIA detector enumerates each MIG profile's full empty-card
      `GetGpuInstancePossiblePlacements` once and stores it in `Placements` (goes into `Devices.Spec`).
    - **Reconciler (F3):** in `DevicesReconciler.Reconcile`'s wholesale Status build (the `applyAllocatedStatus`
      pass), for each MIG card union the live Pods' `AllocatedPhysicalPlacements` → occupied, set `AllocatedProfiles`
      (count by profile) and `RemainingProfiles = device.ComputeRemainingProfiles(occupied, Placements)`. **No NVML; pure
      arithmetic**, folded inside the single Status write (not stomped).
    - Acceptance: id-map table tests pass under `binding/nvml`, free-count table tests under `pkg/device`; a
      reconciler (fake-client) test — non-MIG card → empty ledgers; a MIG card with cached `Placements` and two
      annotated same-profile placements at different slots → correct `AllocatedProfiles`/`RemainingProfiles` (matches
      the A100 worked example); ledger **survives a second reconcile**; `Remaining` overstate-only (never below the
      true value for seen placements); no NVML in tests. Verify:
      `go test ./binding/nvml/... ./pkg/device/... ./pkg/deviceplugin/... ./pkg/devicemanager/...`.

> **Checkpoint B:** the ledger flows into `Devices.Status` by annotation-merge + arithmetic (empty occupied until
> T7 writes placements); the reconciler does no device I/O. Nothing consumes the ledger for placement yet.

**Phase C — Admission path (validate + gate), still no on-node create**

[ ] **T5: F5 — Pod + Instance webhook validation + units fold.**
    - `pod.go`/`instance.go`: `mig-<profile>` in the sliced family; value==1; mutually exclusive with the three
      logical keys; **≤1 distinct profile per Pod** (containers + initContainers); profile exists in the pool's
      Instance-Type-Detail `Physical.Profiles` + count ≤ ceiling → else reject; fold `units =
      MemoryMibToUnits(profile.MemoryMib, cardVRAM)` from the Detail (same fold as the soft memory-mib path);
      cover initContainers; empty-Detail → retryable reject.
    - Acceptance: F5 table tests (valid fold; `mig:2` reject; mig+memMib reject; two-profile Pod reject; unknown
      profile reject; initContainer validated; empty-Detail retryable). Verify: `go test ./pkg/worker/webhooks/worker/...`.

[ ] **T6: F6 — AdmissionCheck profile-aware feasibility (Retry-only).**
    - `node_devices_admission.go`: `cardRequest.profile` (scan Containers + InitContainers); mig feasibility
      branch (`PhysicalSliced.Profiles` non-empty ∧ `Mode∈{None,Sliced}` ∧ `RemainingProfiles[profile]≥1`); "ledger
      not ready" Retry; logical branch excludes MIG cards.
    - Acceptance: F6 table tests (≤N Ready / >N Retry; `Remaining==0` Retry not Reject; ledger-not-ready distinct
      Retry; MIG excluded from logical; Exclusive card not a mig candidate; init-only counted). Verify:
      `go test ./pkg/worker/controllers/worker/...`.

> **Checkpoint C:** a `mig-<profile>` request is validated, quota-folded, and placement-gated; no card is
> created yet (Allocate would fail — expected until T7).

**Phase D — Actuation (the hot path)**

[ ] **T7: F4 — device-plugin `Allocate` MIG branch (create/reuse under per-card lock + ownership marker).**
    - Per-card lock guarding create/marker-write; reuse-or-`CreateGpuInstanceWithPlacement`+`CreateComputeInstance`
      (T1 slot-pick now from `binding/nvml`, T2 wrappers); atomic on-disk marker
      `podUID→{card,giId,ciId,migUUID,profile,start:size}` written in-section; **record the chosen
      `{profile, start:size}` into the Pod's allocation annotation (`AllocatedPhysicalPlacements`) — the occupied
      source T4's reconciler reads to derive `Remaining`**; NVIDIA Responder MIG branch injects only
      `NVIDIA_VISIBLE_DEVICES=<MIG UUID>` (no libvgpu/CUDA_DEVICE_*); CI-fail → destroy GI; roll back on
      marker/annotation-patch failure. The NVML create/destroy lives behind a `_linux.go`/`_other.go` seam (the
      pure slot-pick + marker + annotation-encode logic is platform-independent + seam-testable).
    - Acceptance: seam tests for slot-pick, reuse-vs-create, marker + annotation round-trip; concurrent same-card
      Allocate → no double-create/collision, sibling not blocked; crash-then-retry rebinds. Verify:
      `go test ./pkg/deviceplugin/... ./pkg/devicemanager/allocator/nvidia/...`.

[ ] **T8: F4 — reclaim loop (destroy CI→GI, bounded IN_USE retry, attribution self-check, orphan GC).**
    - Extend `RunSlicedReclaimLoop`/`reclaimer` for MIG: destroy from marker with `reclaimMaxMisses` debounce;
      IN_USE → partial-failure retry (miss counter not cleared) + condition at the bound; self-check marker
      `podUID` vs Pod annotation before destroy; marker-less GI GC only when card drained; per-card locks so
      sibling cards are unblocked.
    - Acceptance: seam tests — dead-Pod GI destroyed after debounce; IN_USE bounded-retries + condition;
      mis-attributed marker does not destroy a live instance; orphan GC only on drained card. Verify:
      `go test ./pkg/deviceplugin/... ./pkg/devicemanager/allocator/...`.

> **Checkpoint D:** end-to-end create/reclaim works against a real card (H100) — validated in T10's e2e;
> full `go test ./...` green.

**Phase E — Capacity key, docs, real-card**

[ ] **T9: F7 — advertise `.sliced.mig-<profile>` node capacity keys + verify Kueue coverage.**
    - `node_capacity.go`: emit `<base>.sliced.mig-<profile>` = `Detail.Physical.Profiles[name].Count`; add the
      suffix to `slicedCapacitySuffixes`; extend `slicedSignature` so a capability change re-enqueues; watch
      stays capability-only. Verify (build-time) that `.sliced.mig-*` gets the soft keys' credits-based Kueue
      treatment; extend `buildResourceGroups` only if soft keys are individually CQ-covered.
    - Acceptance: table tests (MIG group advertises the keys; non-MIG none; last-MIG-off reverse-patches null;
      four soft keys unchanged on non-MIG — criterion 4). Verify: `go test ./pkg/worker/controllers/worker/...`,
      `make lint`.

[ ] **T10: F8 — docs + H100 real-card e2e.**
    - Docs: allocation user contract (request shape, profile-name requirement, value==1 / one-profile-per-Pod,
      MIG-must-go-through-Kueue, reclaim/IN_USE), with the profile + placement tables copied in (no research
      links).
    - H100 e2e on `testing/infra`: enable MIG → restart DeviceManager → `Remaining` appears → submit `mig-1g.10gb`
      LocalQueue workload → admits, scheduler-fit + kubelet admission verified, container sees the MIG device
      via `nvidia-smi`, `libvgpu.so` absent, `Remaining` decrements → delete → instance destroyed, `Remaining` restores;
      assert the `/8` denominator on the actual card generation. A100 reset path documented, validated externally.
    - Acceptance: doc renders (generic "a Kubernetes cluster"); H100 e2e passes on the H100 host.

> **Final checkpoint:** `grep` shows no stale symbols; `go build ./...`, full `go test ./...`, `make lint`,
> `make generate`/`make generate nvml` all clean.

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
- **Factor the novel logic into pure functions** — the profile↔GI/CI-id mapping (`binding/nvml`), the pure
  free-count math (`pkg/device`), and the units fold — since there is no local MIG hardware and NVML
  create/destroy is hardware-only. The NVML I/O wrappers (F1) are build-level only locally.
- **The F3 ledger is fake-client-testable with NO seam** — the reconciler derives it from `Devices.Spec`
  (cached `Placements`) + Pod annotations (`AllocatedPhysicalPlacements`), pure arithmetic; the test injects those as
  fixtures. **Add a fake NVML seam only for the device-plugin MIG Allocate/reclaim (F4)** so create/reuse/destroy
  decisions, the marker + annotation round-trip, and the reclaim debounce are table-testable without hardware
  (mirror the MetaX/Cambricon reclaimer seam + temp marker dir).
- **Snapshot the pre-change non-MIG `.sliced.*` output** for a representative fixture before T9, to prove the
  four keys and admission verdicts are byte-identical after (criterion 4).

#### Unit tests
Per-package targets (percentages are targets to meet or exceed; concrete names recorded after the PR merges):
- `binding/nvml`: `2026-07-22` - `80%` for the profile↔GI/CI-id mapping (all six kept profiles); the cgo
  wrappers stay build-level (real calls hardware-only)
- `pkg/device`: `2026-07-22` - `80%` for the pure free-count geometry (ComputeRemainingProfiles A100 worked example /
  empty / fragmented; ProfileCountSlice name-sort + nil-empty)
- `pkg/devicemanager/detector/nvidia`: `2026-07-22` - `80%` (detect-time `Placements` enumeration/caching; the
  units fold; profile derivation — geometry now lives in `binding/nvml`)
- `pkg/devicemanager/allocator/nvidia`: `2026-07-22` - `75%` (MIG create/reuse decision, marker + annotation
  round-trip, reclaim destroy/IN_USE/self-check/orphan-GC on the fake seam; MIG env = MIG UUID only, no libvgpu)
- `pkg/deviceplugin`: `2026-07-22` - `75%` (reconciler MIG ledger by annotation-merge + `ComputeRemainingProfiles`,
  folded into the wholesale Status build and not stomped on a second reconcile, overstate-only; Allocate mig
  branch under per-card lock; concurrent same-card no-collision)
- `pkg/worker/webhooks/worker`: `2026-07-22` - `80%` (F5: fold, value==1, mig/logical exclusivity, ≤1 profile
  per Pod, unknown-profile reject, initContainers, empty-Detail retryable)
- `pkg/worker/controllers/worker` (node_devices_admission): `2026-07-22` - `80%` (F6 feasibility matrix,
  ledger-not-ready Retry, logical excludes MIG, init-only counted)
- `pkg/worker/controllers/worker` (node_capacity): `2026-07-22` - `80%` (F7 mig capacity keys, reverse-patch,
  non-MIG four-key identity)
- `pkg/nodefeature`: `2026-07-22` - keep current % (new resource-name helper + suffix recognition)
- `api/worker/v1alpha1`: `2026-07-22` - `None` (generated deepcopy/protobuf; contiguous-number + comparability
  compile-check rides the consuming package)

#### Integration tests
Fake-client reconciler/webhook tests (project convention — no envtest cluster):
- **Non-MIG numeric identity (regression guard):** a non-MIG NVIDIA group → the four `.sliced.*` keys and
  exclusive/shared/soft-sliced admission are byte-identical to pre-change (criterion 4).
- **Ledger not stomped (NEW-5):** a MIG card with cached `Placements` + Pod MIG annotations reconciles to a
  ledger; a second reconcile (extra non-MIG Pod annotation) recomputes it in the wholesale build — assert
  `RemainingProfiles`/`AllocatedProfiles` survive (recomputed, not a stompable second write).
- **Occupied-interval reconstruction (C3):** two same-profile Pods annotated at different legal slots →
  per-card `Remaining` reflects their actual placements, not the empty-card ceiling. Pure arithmetic, no NVML.
- **`Remaining` overstate-only (NEW):** for the placements the reconciler sees, computed `Remaining` never drops below the
  true value (missing occupancy only inflates `Remaining`), so the AdmissionCheck never falsely strands capacity.
- **Ownership + restart (C2):** create GI+CI + marker; simulate Allocate response loss / process restart before
  the response; retry Allocate → rebinds the same GI, no double-create, no orphan.
- **Reuse-vs-GC ordering (NEW-4):** crash after GI create but before the response; on kubelet retry the GI is
  rebound and the reclaim loop does not destroy it mid-retry (debounce > retry window).
- **Concurrent same-card Allocate (C4):** two Allocates for one profile on one card with `Remaining==1` → per-card
  lock admits one, the other fails safe → recreate; a sibling card is not blocked while card A creates.
- **Reclaim matrix (C10):** dead-Pod GI destroyed after debounce; residual `IN_USE` → bounded retry + condition,
  sibling-card Allocate proceeds; node-reboot (NVML zero GIs, stale markers) → ledger realigns, markers GC'd;
  mis-attributed marker (reordered admission, NEW-12) → self-check prevents destroying a live instance.
- **Units-fold quota semantics (C7) + MIG↔logical parity:** a MIG `mig-<profile>` and a soft
  `.sliced.memory-mib` request of the profile's VRAM fold to the **identical** `.sliced.units`/credits (the
  non-conflict property); and, matching the worked example, on an H100 with CQ quota `= cards×D`, 8× `1g.10gb`
  on one card → 7 admit, the 8th quota-fits (credits) but Retries on `Remaining==0` — assert this documented
  compute-limited under-charge; same-memory profiles (`1g.20gb`/`2g.20gb`) charge identically.
- **Multi-profile Pod reject (C8):** a Pod with two containers naming different profiles, and an init-container
  profile ≠ app-container profile → ingress reject.
- **Rollout skew (NEW-17):** operator at spec-2, DaemonSet at spec-1 format (no `Placements` cached, no MIG
  annotation) → mig workload Retries with a "ledger not ready" message (no Reject), then admits once the
  DaemonSet rolls.
- **Kueue coverage (NEW-9):** a mig workload on a LocalQueue whose CQ gives the manufacturer credits quota →
  reaches F5/F6 (not stranded in flavor assignment); the mig key gets the soft keys' credits-based treatment.
- **Comparability/contiguity (from spec 1):** `InstanceTypeSpec` still a valid map key; `AcceleratorAllocation`
  used only on status paths; `generated.proto` numbers contiguous after the F2 additions.

#### e2e tests
- **H100 real-card (Hopper+, reset-free) — the load-bearing proof:** on `testing/infra`, enable MIG on a card,
  restart the DeviceManager, assert `Remaining` appears; submit a `mig-1g.10gb` LocalQueue workload; assert
  scheduler-fit + kubelet admission + `Devices.Status` accounting, the container sees exactly the `1g.10gb` MIG
  device via `nvidia-smi`, `libvgpu.so` is absent, `Remaining[1g.10gb]` decrements; delete → instance destroyed,
  `Remaining` restores; assert the MIG `.sliced.units` fold equals `MemoryMibToUnits(profile.MemoryMib, cardVRAM)`
  (identical to a same-VRAM soft request) on the actual card generation; note the mode is non-persistent across
  reboot (Hopper+). This is the observable proof of criteria 1/2/5/6 and the Kueue-coverage
  path (NEW-9) + capacity-key allocatable path (C6).
- **A100 (Ampere, reset-required) — deferred/external:** no A100 in this environment; the reset-required enable
  sequence is documented for administrators and validated externally, not gated on this spec (Open Question 4).

## Alternatives

- **Shell out to a bundled `mig-parted` binary (HAMi's approach)** — rejected. HAMi `go install`s
  `mig-parted@v0.12.2` into its image and execs `nvidia-mig-parted apply -f /tmp/migconfig.yaml`, holding a
  process lock and `klog.Fatalf`-ing the plugin on failure; `mig-parted`'s `apply` clears the whole card and
  permutation-retries (destructive). We already have a cgo binding system; direct NVML with
  `CreateGpuInstanceWithPlacement` is incremental, transactional against our own ledger, needs no extra binary,
  and avoids `mig-parted`'s no-stable-library-API caveat.
- **Anchor MIG requests on a fraction, auto-translating to a profile (HAMi's user surface)** — rejected (spec
  1's decision, restated): the mapping is many-to-one, non-portable across generations, and a scalar cannot
  defend stranded capacity. Profile-name anchoring matches NVIDIA `mixed`, the DRA driver, and KEP-4815.
- **Make the AdmissionCheck Reject statically-unsatisfiable requests** — rejected in favor of rejecting at the
  ingress webhook. The webhook can check profile existence and count-vs-ceiling synchronously at submission
  (fail fast, clear error), preserving the AdmissionCheck's simple never-Reject/Retry-only contract and its
  small blast radius (no risk of a feasibility bug evicting a healthy soft-slice Workload).
- **Pass the chosen (card, profile) from AdmissionCheck to the device-plugin via a Pod annotation** — rejected.
  The profile is already in the container's `.sliced.mig-*` request; card+slot selection is the device-plugin's
  own concern under `allocateMutex`. An annotation would couple the decision to a fragile Workload→Pod
  propagation path for no benefit.
- **Store the placement `Remaining` map statically in the capability type** — rejected. `Remaining` depends on live
  occupancy; storing it statically would immediately go stale. What *is* cached in the capability is the static,
  occupancy-independent **possible-placement set** (`Placements`, F2); `Remaining` is recomputed every reconcile from
  it minus the annotation-derived occupied (F3) and published to `Status` (observed), where it belongs.
- **Compute `Remaining` from live NVML inside the reconciler** — rejected (Decision 2). It is accurate (catches
  out-of-band/orphan GIs immediately) but puts ~50 blockable NVML ioctls per MIG card into the level-based
  reconciler's hot path, stalling all modes' Status updates. The annotation-merge + detect-cache path keeps the
  reconciler pure arithmetic; the accepted cost is transient `Remaining` overstatement caught at `Allocate`/reclaim.
- **Add a soft reservation/TTL layer to the AdmissionCheck for the race** — deferred, not built. The existing
  `allocateMutex` arbitrates; a reservation layer is added only if real-hardware contention proves it
  necessary (Ask-first boundary).

## Open Questions

1. **CI profile/engine-profile mapping per kept profile** — resolved to a T1 deliverable: the exact
   `NVML_COMPUTE_INSTANCE_PROFILE_*` / `NVML_COMPUTE_INSTANCE_ENGINE_PROFILE_*` constants for each C==G profile
   are pinned in T1's mapping table against `mig-parted`'s observed sequence + the NVML `const.go` ids, and
   confirmed on the H100 e2e (T10). (F1/F4)
2. **Ledger publish cadence** — resolved by Decision 2: the ledger is recomputed each reconcile from annotations
   + cached `Placements` and written only through the existing `DeepEqual` gate, so there is no separate publish
   cadence or NVML burst to tune. (F3)
3. **Reclaim ownership** — resolved: reclaim runs in a **separate worker** (`RunSlicedReclaimLoop`, as
   MetaX/Cambricon do) with per-card locks, not co-located with `Allocate`, so an `IN_USE` retry never blocks a
   sibling card's allocation (T8/F4). (F4)
4. **A100 e2e coverage** — the Ampere reset-required mode path is documented but validated externally; if an
   A100 becomes available, add its reset-orchestration e2e as a follow-up (not blocking this spec). (F8)
5. **A status-ledger Devices watch for faster AdmissionCheck convergence** — deferred, Ask-first: F6 converges
   via Kueue's eviction+requeue (correct but ≥30s/cycle with quota held). A Status-`RemainingProfiles`-filtered watch
   enqueuing affected Workloads would cut the latency; add only if the eviction-cycle cost proves material. (F6)
6. **DRA / KEP-4815 evolution** — if/when the mig-* extended-resource route migrates to ResourceClaim + counter
   sets, whether the per-card `Remaining` ledger maps directly onto counter-set accounting is a future-direction
   question, not built here.
