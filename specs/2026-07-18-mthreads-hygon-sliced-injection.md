# Spec: Vendor Soft-Slicing Runtime Injection — MThreads (QoS env) and Hygon (vdev.conf) Allocator Branches

Status: Built
Type: Feature

## Summary
The capability spec `2026-07-16-accelerator-slicing-capability-and-pool-feedback` made all six accelerator
vendors *advertise* a vendor-aware sliced capacity, but explicitly left Cambricon / Hygon / MThreads / MetaX
without any real `AllocationMode==Sliced` injection branch — those four vendors do not even register a Sliced
device-plugin server today (`grep DeviceAllocationModeSliced` hits only NVIDIA and Ascend). This spec builds the
real sliced injection branch for the two **stateless / semi-stateless** vendors — **MThreads** (pure QoS
environment variables) and **Hygon** (a per-pod `vdev.conf` + CU bitmask, host-mounted) — following the shipped
NVIDIA / Ascend allocator template and reusing the shipped `SlicedCoresPercent` / `SlicedMemoryMib` helpers and
the level-based `podDirGC`. Registering the Sliced server for each also activates the `.sliced` advertisement the
capability spec sized but never wired for these two vendors, so their full sliced pipeline (advertise → schedule →
allocate → inject) goes live.

No vendor test hardware is available, so this delivers **build- and unit-test-verifiable** code — everything
compiles and unit-tests locally on `darwin` and neither vendor needs a new Dockerfile builder stage or an
operator-bundled library (Hygon reads host-mounted DTK/hyhal; MThreads relies on the host sGPU kmod + container
runtime). Real-hardware validation that the vendor runtime consumes the injected env / config is a deliberate
later phase. The two **stateful** vendors (MetaX sysfs `sgpu` subdevices, Cambricon cnDev sMLU instances) — which
require a subdevice create/track/destroy lifecycle plus a per-vendor reclamation reconciler — are an explicit
follow-up spec.

## Motivation
### Goals
- **A sliced MThreads / Hygon container gets real per-slice isolation**, not a whole-card passthrough: a hard VRAM
  cap and a compute budget derived from the container's `.sliced.cores-percentage` / `.sliced.memory-percentage` /
  `.sliced.memory-mib` request, translated to each vendor's runtime isolation primitive.
- **MThreads (lightest, stateless):** set `MTHREADS_QOS_MEMORY_LIMIT` (bytes — the hard VRAM cap) and
  `MTHREADS_QOS_COMPUTING_POWER_WEIGHT` (a relative compute weight from `cores%`), keeping `MTHREADS_VISIBLE_DEVICES`.
  Because MThreads compute is a *relative weight*, not a hard cap (matching the shipped `CoresPercentageOvercommit=false`
  for MThreads), `.sliced.cores-percentage` there is best-effort and must be documented as such.
- **Hygon (semi-stateful):** translate `cores% → CU count → CU bitmask` against the card's free CUs, write a
  per-pod `vdev.conf` (`PciBusId` / `cu_mask×2` / `cu_count` / `mem: <MiB> MiB` / `device_id` / `vdev_id` /
  `pipe_id` / `enable:1`) into the pod work dir, and mount it to `/etc/vdev/docker/` alongside the DTK/hyhal
  runtime dirs; the host DTK/hyhal user-space runtime reads it. A whole-card request (`cores% ≥ 100` AND
  `memMiB ≥ card VRAM`) still writes a full-mask / full-memory `vdev.conf` occupancy record (never a config-less
  hole the on-disk scanner cannot see).
- **Hygon compute is a spatial partition, not overcommit (C1 reconciliation).** The `vdev.conf` CU bitmask assigns
  disjoint CU bits (sum ≤ one card), so the Hygon detector flips `CoresPercentageOvercommit` from `true` to `false`
  and `.sliced.cores-percentage` rescales to `cards × 100`. The capability spec deferred this exact judgment to
  "when each vendor's real injection lands" — i.e. here.
- **Follow the shipped NVIDIA / Ascend template exactly:** `New()` gains a `!opts.NoSliced`-gated Sliced server →
  `GetContainerAllocateResponse` branches on `AllocationMode==Sliced` → the branch reuses the public
  `deviceplugin.SlicedCoresPercent` / `deviceplugin.SlicedMemoryMib` helpers for `(cores%, memMiB)`. No changes to
  `pkg/nodefeature`, the Pod webhook, Kueue credits, or the `.sliced` sizing in `pkg/deviceplugin`.
- **State is level-based and restart-surviving.** Hygon's `vdev_id` / `pipe_id` / CU-bitmask allocation is derived
  by scanning the on-disk per-pod `vdev.conf` files (the same pattern as Ascend's `lowestFreeVNPUID`), so the
  existing `podDirGC` reclaims a dead pod's work dir and its slot is freed on the next Allocate scan — **no new
  reconciler and no reliance on a (nonexistent) Release callback.** MThreads holds no state.
- **Success criteria (code-only, no hardware):**
  1. MThreads: a container requesting `cores% = 8`, `memory-percentage = 50` on a 48 GiB card yields envs
     `MTHREADS_QOS_MEMORY_LIMIT = 25769803776` (24 GiB in bytes), `MTHREADS_QOS_COMPUTING_POWER_WEIGHT = 8`,
     `MTHREADS_VISIBLE_DEVICES = <ids>`; an absent `cores%` defaults the weight to 100.
  2. Hygon: a container requesting `cores% = 25` on a 64-CU card yields `cu_count = 16` and a `vdev.conf` whose
     `cu_mask` packs 16 free CU bits not already used by another live slice on the same card; a second concurrent
     slice on the same card packs the *next* 16 free bits and takes the next `vdev_id` / `pipe_id`.
  3. Hygon: `cores% ≥ 100` AND `memMiB ≥ card VRAM` still writes a full-mask / full-memory `vdev.conf` occupancy
     marker (not a config-less card), plus the whole-card device nodes + DTK/hyhal mounts.
  4. `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./...` and `make lint` are clean; the two allocator packages'
     table tests pass; no `make generate` is needed (no API type change).

### Non-Goals
- **MetaX and Cambricon real injection.** They are stateful (MetaX writes sysfs `sgpu/create` subdevices + injects
  `METAX_SGPUS`; Cambricon drives cnDev sMLU profile/instance lifecycle via `binding/cndev`) and need a
  create/track/destroy lifecycle plus reclamation. Per the scope decision they are a **follow-up spec**, and per
  the reclamation decision that follow-up uses **a per-vendor reconciler** (a level-based loop that lists live
  sysfs/cndev subdevices and destroys orphans whose pod is gone) rather than extending `podDirGC`.
- **Changing the sliced advertisement / reporting math.** `.sliced = cards × maxSlices`, `.sliced.units = cards × D`,
  `.sliced.cores-percentage`, `.sliced.memory-*` and the Kueue credit fold-down are all owned by the capability
  spec and unchanged; this spec only makes the two vendors *serve* the `.sliced` resource (by registering the
  Sliced server) and *inject* real isolation at Allocate. **One deliberate exception:** flipping Hygon's
  `CoresPercentageOvercommit` to `false` (C1), which rescales only Hygon's `.sliced.cores-percentage` to
  `cards × 100` — the reconciliation the capability spec explicitly deferred to this follow-up.
- **Real-hardware / E2E validation of the vendor runtime consuming the injected env / config.** No test hardware
  exists. MThreads' `MTHREADS_QOS_*` env semantics (single-source vendor doc) and Hygon's "DTK/hyhal reads
  `vdev.conf`" (inferred, not proven in device-plugin source) MUST be validated on real hardware before GA.
- **New Dockerfile xbuild stages / operator-bundled libraries.** Neither vendor needs one (unlike NVIDIA's
  `libvgpu.so` / Ascend's `libvruntime.so`). Hygon uses host `/opt/hyhal` + DTK; MThreads uses the host sGPU kmod +
  MThreads container runtime.
- **Changing NVIDIA / Ascend.** Their branches are untouched.

## Proposal
Add a real sliced injection branch to the MThreads and Hygon allocators, using the established per-vendor template
and the shipped shared plumbing, so a sliced pool of either vendor is functional end-to-end on real hardware once
validated.

### MThreads — pure QoS environment variables (stateless)
`New()` registers a `!opts.NoSliced`-gated Sliced server. In `GetContainerAllocateResponse`, the
`AllocationMode==Sliced` branch reads `cores%` (default 100) and `memMiB` (percentage-preferred, capped at the
group's per-card VRAM) via the shared helpers and returns:

```
MTHREADS_VISIBLE_DEVICES            = join(allocated ids)          # already emitted for non-sliced
MTHREADS_QOS_MEMORY_LIMIT           = memMiB * 1024 * 1024         # bytes — hard VRAM cap
MTHREADS_QOS_COMPUTING_POWER_WEIGHT = cores%                       # relative weight (best-effort, NOT a hard cap)
```

No files, no mounts, no device nodes, no GC. The host sGPU kmod + MThreads container runtime are a documented
prerequisite (the env is a no-op without them); a missing runtime is not a hard Allocate failure because the env
injection is harmless.

### Hygon — per-pod `vdev.conf` + CU bitmask (semi-stateful, host-mounted)
`New()` registers a `!opts.NoSliced`-gated Sliced server. The `AllocationMode==Sliced` branch, **per allocated
card**:
1. Reads `cores%` (default 100) and `memMiB` via the shared helpers.
2. `cuCount = ceil(cores% × group.Cores / 100)` (`group.Cores` = the detector's `ComputeUnitCount`), capped at
   `group.Cores` and floored so a positive percent never yields 0 CUs.
3. Packs `cuCount` lowest **free** CU bits into a 128-bit `(cu_mask1, cu_mask2)` — disjoint from bits already used
   by another live slice on the same card (a spatial partition, sum ≤ one card; this is why C1 flips overcommit to
   `false`) — and allocates a node-wide `vdev_id` (≤ 200) + a `pipe_id` unique per canonical card BDF (≤ 20).
4. Renders one `vdev<i>.conf` per allocated card into `PodWorkDir(pod.UID, ctr.Name)/etc/vdev/docker/` and returns
   the mounts (that directory → `/etc/vdev/docker/`, `/opt/dtk` → `/opt/hygondriver`, keep `/opt/hyhal` ro) + the
   device nodes (`/dev/kfd`, `/dev/mkfd`, per-card `/dev/dri/card*`, `renderD*`). A whole-card slice
   (`cores% ≥ 100` AND `memMiB ≥ group.Memory`) still writes a full-mask / full-memory `vdev.conf` **occupancy
   marker** — never a config-less card the on-disk scanner cannot see.

**Concurrency + durability (cross-check-hardened).** The responder runs *outside* the reconciler's `allocateMutex`
(`pkg/deviceplugin/server.go` invokes it after that closure returns) and kubelet does not reliably serialize
`Allocate` (the code anticipates concurrent Kueue batches), so the whole scan → validate → allocate → write runs
under a package-level `sync.Mutex` and publishes each `vdev.conf` via a temp-file + atomic rename; parsing is
**fail-closed** (a corrupt / partial / duplicate / out-of-range conf errors rather than silently freeing its
`vdev_id` / `pipe_id` / CU mask). The `vdev_id` / `pipe_id` / used-CU-bit sets are derived by scanning the on-disk
per-pod `vdev.conf` files (level-based, restart-surviving); a re-allocation of the same card + request reuses its
existing ids/mask (idempotent). Exhaustion (no free `vdev_id` / `pipe_id` / CU bits) returns a loud error →
reschedule. The per-card ≤ 4-slice bound is already enforced upstream by the `LogicalSliced{4,…}` token pool.

The existing `podDirGC` (fed the live pod-UUID set through the Sliced server's ListAndWatch notifier) reclaims the
pod work dir when the pod is gone, freeing every slot on the next scan; no separate reconciler is added. A
`vdev.conf` under a still-terminating pod's dir is intentionally retained until the pod object is gone, so the scan
keeps treating its ids/CUs as occupied.

### User Stories
#### Story 1 — MThreads slice gets a hard VRAM cap
As a **workload user**, when I request a sliced MThreads device with a memory percentage, I want the container's
VRAM hard-capped to that share via `MTHREADS_QOS_MEMORY_LIMIT`, so that co-located slices cannot exceed their VRAM
budget.

#### Story 2 — MThreads compute is an honest relative weight
As a **cluster operator**, I want `.sliced.cores-percentage` on MThreads mapped to `MTHREADS_QOS_COMPUTING_POWER_WEIGHT`
as a documented *relative weight* (not a hard cap), so that I understand a MThreads compute slice is best-effort
while its memory slice is hard.

#### Story 3 — Hygon slice gets a CU partition + VRAM cap
As a **workload user**, when I request a sliced Hygon device, I want a `vdev.conf` that reserves a `cores%`-derived
CU bitmask and a `memMiB` VRAM cap and is read by the host DTK/hyhal runtime, so that my slice is spatially
partitioned from co-located slices.

#### Story 4 — A Hygon whole-card slice still records occupancy
As the **platform**, when a Hygon sliced request resolves to a whole card (100 % compute and full VRAM), I want a
full-mask / full-memory `vdev.conf` occupancy marker still written (not a config-less card), so that the on-disk
scanner sees the card as taken and never double-books it on a multi-card node.

#### Story 5 — A dead Hygon slice frees its vdev slot automatically
As the **platform**, when a pod holding a Hygon slice disappears, I want its work dir reclaimed by the existing
level-based GC and its `vdev_id` / `pipe_id` / CU bits freed on the next Allocate scan, so that slots never leak
across restarts and no Release callback is needed.

#### Story 6 — The translation logic is verifiable without hardware
As a **maintainer**, I want the per-vendor env / config translation (byte conversion, CU rounding, bitmask packing,
`vdev.conf` rendering, on-disk slot derivation) implemented as pure, table-tested functions, so that the branch is
correct and reviewable before any hardware exists.

### Core Features & Acceptance Criteria
| # | Feature | Acceptance |
|---|---------|-----------|
| F1 | MThreads Sliced server + QoS-env branch | `mthreads/deviceplugin.go`: `New()` registers a `!opts.NoSliced` Sliced server; `GetContainerAllocateResponse` gets an `AllocationMode==Sliced` branch emitting the three envs above from `SlicedCoresPercent` / `SlicedMemoryMib`. Non-sliced modes unchanged. |
| F2 | Hygon Sliced server + `vdev.conf` branch | `hygon/deviceplugin.go`: `New()` registers a `!opts.NoSliced` Sliced server; the Sliced branch computes `cuCount`, packs a CU bitmask, allocates `vdev_id`/`pipe_id`, renders + mounts `vdev.conf`, and keeps the device nodes + DTK/hyhal mounts. |
| F3 | Hygon level-based slot allocation + GC reuse | `vdev_id`/`pipe_id`/CU-bit "used" sets are derived by globbing the on-disk per-pod `vdev.conf` files (restart-surviving); the existing `podDirGC` reclaims the work dir; no new reconciler. Exhaustion → a loud Allocate error. |
| F4 | Hygon whole-card occupancy marker | `cores% ≥ 100` AND `memMiB ≥ group.Memory` still writes a full-mask / full-memory `vdev.conf` occupancy marker (never a config-less hole the on-disk scanner misses), plus the whole-card device nodes + DTK/hyhal mounts. |
| F5 | Code-only tests | Table-driven tests for the pure translation functions (MThreads env map incl. default cores%; Hygon `cuCount` rounding, bitmask packing avoiding used bits, `vdev.conf` render, on-disk slot derivation using a temp `OperatorPodsDir`, `-race` concurrency, fail-closed parsing, whole-card marker). `go build ./...` + `make lint` clean; no `make generate`. |
| F6 | Hygon compute model = spatial partition (C1) | The Hygon detector sets `CoresPercentageOvercommit=false` (disjoint CU bitmask); `.sliced.cores-percentage` rescales to `cards × 100` via the existing overcommit=false branch (no `node_capacity.go` change). |

### Notes / Constraints / Caveats
- Go + controller-runtime; local `darwin/arm64`, `CGO_ENABLED=1`, `GODEBUG=gotypesalias=0`. Follow the Go /
  Kubernetes / testing conventions in `CLAUDE.md`. No API type change → **no `make generate`**, no image build.
- **Vendor facts (from the research report, grounding the branch):**

  | Vendor | Compute key → primitive | Memory key → primitive | State | Isolation executed by |
  |---|---|---|---|---|
  | MThreads | `cores%` → `MTHREADS_QOS_COMPUTING_POWER_WEIGHT` (relative weight) | `memMiB×1Mi` → `MTHREADS_QOS_MEMORY_LIMIT` bytes (hard) | none | host sGPU kmod + MThreads container runtime |
  | Hygon | `cores%` → `round(× ComputeUnit/100)` CU bits in `cu_mask×2` | `memMiB` → `mem: <MiB> MiB` in `vdev.conf` | per-pod file + node vdev/pipe pools | host DTK/hyhal user-space runtime reading `vdev.conf` |
- The two shared helpers already produce the exact inputs: `SlicedCoresPercent(ctr, coresRes)` (default 100,
  overcommit-allowed) and `SlicedMemoryMib(ctr, memPctRes, memMibRes, cardVRAMMib)` (percentage-preferred, capped,
  errors if neither set). No per-vendor memory/percentage math is re-derived.
- **Enabling the Sliced server is a behavior change for these two vendors:** it activates the `.sliced` resource
  (sized `cards × maxSlices` by `pkg/deviceplugin`) and the presence-gated `.sliced.*` node capacities from the
  capability spec, so a node with MThreads/Hygon now schedules and admits sliced workloads through the new branch.
  This closes the gap where the capability spec sized `.sliced` but no Sliced server existed to serve it.
- **Hygon bounds:** ≤ 4 slices/card (upstream `LogicalSliced{4}` token pool), ≤ 200 vdev/node, ≤ 20 pipe/card
  (enforced in-branch by the loud-fail on exhaustion). The stale `# 60%` comment in the vendor README is wrong; the
  code treats `dcucores` as a literal percent.
- **Hygon multi-card (resolved):** an operator sliced allocation spanning multiple cards writes one `vdev<i>.conf`
  per allocated card (each independently slotted); the whole-card marker is evaluated per card.

### Boundaries
- **Always:** follow the NVIDIA/Ascend allocator template; reuse `SlicedCoresPercent` / `SlicedMemoryMib`; keep
  Hygon's slot allocation level-based and on-disk-derived (restart-surviving); keep the `.sliced` / `.sliced.*`
  advertisement values sourced from the capability spec (never re-hard-code them here); isolate the pure
  translation logic into table-tested functions; run `make lint` + the two package tests + a whole-module `darwin`
  build.
- **Ask first:** adding any Dockerfile xbuild stage or operator-bundled library for either vendor (should not be
  needed); changing the MThreads compute mapping beyond `weight = cores%`; changing the shared `podDirGC` signature
  or the `pkg/deviceplugin` `.sliced` sizing; restricting Hygon sliced to single-card.
- **Never:** implement MetaX or Cambricon here (deferred follow-up); use in-memory (non-restart-surviving) slot
  counters; silently overcommit a card when no free `vdev_id` / `pipe_id` / CU slot remains; claim real-hardware
  validation (none exists); expose a whole card to a *partial* sliced request without writing the isolation config.

### Risks and Mitigations
- **MThreads `MTHREADS_QOS_*` semantics are single-source (vendor doc, not code-verified)** → Risk: the runtime may
  expect a different weight scale or env name. Mitigation: code to the documented contract, isolate the encoding in
  a pure function, and flag hardware validation as a release gate; the compute-weight-is-not-a-hard-cap caveat is
  documented to the user.
- **Hygon "DTK/hyhal reads `vdev.conf`" is inferred, not proven in device-plugin source** → Risk: the exact file
  location / schema / consuming component may differ. Mitigation: code to the documented `vdev.conf` schema + mount
  layout, unit-test the rendering, and validate on hardware before GA.
- **CU bitmask spatial packing wrong** → Risk: overlapping masks silently share CUs. Mitigation: unit-test the
  packing against on-disk used-bit sets; fail loudly on exhaustion; hardware validation confirms enforcement.
- **Activating the Sliced server changes scheduling for existing MThreads/Hygon nodes** → intended and consistent
  with the capability spec; documented so operators expect sliced admission to begin.
- **No test hardware** → all verification is build + unit-test + lint; the vendor-runtime consumption is a
  documented follow-up hardware phase, not a silent gap.
- **Concurrent Hygon Allocates race on the on-disk slot scan** (cross-check C2) → the responder runs outside the
  reconciler's `allocateMutex` and kubelet does not reliably serialize `Allocate` (the code anticipates concurrent
  Kueue batches). Mitigate with a package-level `sync.Mutex` over scan → validate → allocate → write + atomic
  temp-file rename, proven by a `-race` unit test. (The shipped Ascend `lowestFreeVNPUID` shares this latent race;
  out of scope to change here, noted for a follow-up.)
- **A corrupt / partial `vdev.conf` frees three coupled resources at once** (cross-check H4) → fail-closed parsing:
  an unparsable / out-of-range / duplicate record errors rather than releasing its `vdev_id` / `pipe_id` / CU mask.
- **A config-less whole-card slice is invisible to the scanner** (cross-check C3) → a whole-card slice still writes
  a full occupancy `vdev.conf`, so no card is silently double-booked on a multi-card node.
- **Responder failure after the durable annotation strands the allocation** (cross-check H6) → pre-existing
  framework behavior (`patchAllocatingPod` precedes the responder; shared with NVIDIA/Ascend). Mitigate by bounding
  concurrency upstream (the `.sliced` token pool makes exhaustion unreachable in practice) and failing closed; the
  level-based reconcile converges once the pod is removed. Fixing the framework ordering is out of this spec's
  surgical scope.

## Design Details
### Commands
```bash
# No API change → no `make generate`, no image build. Whole-module build smoke on darwin (CGO detectors included):
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./...
# Targeted package tests:
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race \
  ./pkg/devicemanager/allocator/mthreads/... ./pkg/devicemanager/allocator/hygon/... ./pkg/deviceplugin/...
# Lint (also fails if the tree is left dirty):
make lint
```
### Project Structure (files in scope)
```
pkg/devicemanager/allocator/mthreads/deviceplugin.go        # F1 add Sliced server + QoS-env branch
pkg/devicemanager/allocator/mthreads/deviceplugin_test.go   # F5 new: QoS env encoding table tests
pkg/devicemanager/detector/hygon/device.go                  # F6 CoresPercentageOvercommit true → false (C1)
pkg/devicemanager/allocator/hygon/deviceplugin.go           # F2/F4 add Sliced server + branch + whole-card marker + mounts
pkg/devicemanager/allocator/hygon/vdev.go                   # F2/F3 pure + package sync.Mutex: cuCount, CU bitmask pack, atomic vdev.conf write, fail-closed on-disk slot derivation
pkg/devicemanager/allocator/hygon/deviceplugin_test.go      # F5 new: rounding/bitmask/render/slot/-race/fail-closed table tests
```
### Code Style
```go
// MThreads sliced branch: memory is a hard byte cap; compute is a relative weight (best-effort).
coresRes := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer)
memPctRes := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer)
memMibRes := nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer)
memMib, err := deviceplugin.SlicedMemoryMib(ctr, memPctRes, memMibRes, int64(group.Memory))
if err != nil {
    return nil, fmt.Errorf("derive sliced memory limit: %w", err)
}
envs := map[string]string{
    "MTHREADS_VISIBLE_DEVICES":            strings.Join(ids, ","),
    "MTHREADS_QOS_MEMORY_LIMIT":           strconv.FormatInt(memMib*1024*1024, 10),
    "MTHREADS_QOS_COMPUTING_POWER_WEIGHT": strconv.Itoa(deviceplugin.SlicedCoresPercent(ctr, coresRes)),
}

// Hygon: cores% → CU count against the card's compute-unit count (group.Cores);
// ceil so a positive percent never rounds down to zero CUs, capped at the card total.
cuCount := min(int(math.Ceil(float64(coresPct)*float64(group.Cores)/100)), int(group.Cores))
```
### Implementation Plan
**Dependency graph.** Task 1 (MThreads) is independent and lands the shared per-vendor template on the simplest,
stateless vendor — `New()` Sliced-server registration, the `AllocationMode==Sliced` responder branch, helper
reuse, and the Ascend-style test harness — so the pattern is proven before the stateful Hygon work. Task 2 is a
**de-risk spike**: the C1 detector flip plus the pure Hygon translation core (`vdev.go`) with the concurrency
guard and fail-closed on-disk slot logic the cross-check flagged as the riskiest surface, verifiable in isolation
under `-race` with no hardware. Task 3 wires that core into the Hygon responder branch and depends on Task 2. Each
task leaves the tree building, linting, and green; checkpoints sit after Task 1 and Task 3.

- [x] **Task 1 — MThreads sliced injection (template + harness).** In `mthreads/deviceplugin.go`, register a
  `!opts.NoSliced` Sliced server in `New()` and add an `AllocationMode==Sliced` branch to
  `GetContainerAllocateResponse` that reads `cores%` (`deviceplugin.SlicedCoresPercent`, default 100) and `memMiB`
  (`deviceplugin.SlicedMemoryMib` over `group.Memory`, error if neither memory dimension is set) and emits
  `MTHREADS_VISIBLE_DEVICES` + `MTHREADS_QOS_MEMORY_LIMIT` (`memMiB × 1024 × 1024` bytes) +
  `MTHREADS_QOS_COMPUTING_POWER_WEIGHT` (`cores%`). Name the previously-ignored `pod` / `ctr` params; non-sliced
  modes unchanged. **Accept:** cores=8 / memory-percentage=50 on a 48 GiB card → the three envs with
  `MTHREADS_QOS_MEMORY_LIMIT=25769803776` and weight `8`; absent cores → weight `100`; no memory dimension →
  Allocate error; sliced vs exclusive/shared/visibility branches stay isolated.
  **Verify:** `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test ./pkg/devicemanager/allocator/mthreads/... && make lint`.
- [x] **Task 2 (de-risk spike) — Hygon translation core + C1 reconciliation.** (a) Flip the Hygon detector's
  `CoresPercentageOvercommit` from `true` to `false` in `pkg/devicemanager/detector/hygon/device.go` (disjoint CU
  bitmask = spatial partition; `.sliced.cores-percentage` rescales to `cards × 100` via the existing
  overcommit=false branch — no `node_capacity.go` change). (b) Add `hygon/vdev.go` with pure, table-tested
  functions guarded by a package-level `sync.Mutex` over the whole scan → validate → allocate → write:
  `cuCount = min(ceil(cores% × group.Cores / 100), group.Cores)` (a positive percent never yields 0); a CU-bitmask
  packer that fills the lowest free bits of a 128-bit `(mask1, mask2)` disjoint from bits used by live slices on the
  same card (validate both words, reject bits ≥ Cores / overlaps / `cuCount > free`); on-disk dual-pool slot
  derivation (`vdev_id` unique node-wide ≤ 200; `pipe_id` unique per canonical card BDF ≤ 20) by globbing the
  per-pod `vdev.conf` files under `OperatorPodsDir`; a `vdev.conf` renderer; atomic temp-file + rename publication;
  **fail-closed** parsing (corrupt / partial / duplicate / out-of-range → error, never silently skipped); idempotent
  reuse of an existing self-config for the same card + request. **Accept:** cores=25 on a 64-CU card → cuCount 16 +
  the 16 lowest free bits; a second same-card slice packs the next 16 bits + next `vdev_id` / `pipe_id`; a different
  card resets `pipe_id` but not `vdev_id`; a `-race` test with concurrent goroutine allocations on one card yields
  distinct ids + disjoint masks; a corrupt conf / unreadable dir / `>128`-CU card / zero-compute request all fail
  closed; exhaustion (200 vdev / 20 pipe / CUs) errors; restart reconstructs the used sets from pre-existing confs;
  the C1 flip sets `CoresPercentageOvercommit=false` (code-review-verified — no hardware-gated detector unit test;
  the downstream `cards × 100` rescale is covered by the existing parameterized `node_capacity` test). **Verify:** `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/devicemanager/allocator/hygon/... ./pkg/devicemanager/detector/hygon/... && make lint`.
- [x] **Task 3 — Hygon sliced responder branch (wiring).** In `hygon/deviceplugin.go`, register a `!opts.NoSliced`
  Sliced server in `New()` and add an `AllocationMode==Sliced` branch that, per allocated card, calls the Task-2
  core to allocate a slot + CU mask, renders one `vdev<i>.conf` into
  `PodWorkDir(pod.UID, ctr.Name)/etc/vdev/docker/`, and returns the mounts (that dir → `/etc/vdev/docker/`,
  `/opt/dtk` → `/opt/hygondriver`, keep `/opt/hyhal` ro) + device nodes (`/dev/kfd`, `/dev/mkfd`, per-card
  `/dev/dri/card*`, `renderD*`). A whole-card slice (`cores% ≥ 100 && memMiB ≥ group.Memory`) still writes a
  full-mask / full-memory `vdev.conf` occupancy marker (no config-less hole). **Accept:** a partial slice writes the
  expected `vdev.conf` bytes (mode 0644) + mounts + device nodes; a whole-card slice writes a marker conf (not
  skipped); a multi-allocated-card request writes one `vdev<i>.conf` per card. **Verify:**
  `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/devicemanager/allocator/hygon/... ./pkg/deviceplugin/... && make lint`.
- *Checkpoints (after Task 1 and Task 3):* `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./...` + `make lint`
  clean + the two allocator packages + `pkg/deviceplugin` + `pkg/devicemanager/detector/hygon` tests green. No
  `make generate` (no API type change). Real-hardware validation of the injected env / `vdev.conf` is a documented
  post-merge phase, not a checkpoint here.
### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
- No Hygon detector unit test exists to update (detectors are hardware-gated and untested by precedent, so a
  literal-asserting test would be extraction-for-testing); the C1 `CoresPercentageOvercommit=false` flip is a
  one-line change verified by code review. Its downstream — the `.sliced.cores-percentage` rescale to `cards × 100`
  under `overcommit=false` — is already covered by the parameterized `TestDesiredSlicedCapacity` (synthetic
  `overcommit` fixtures), which stays green unchanged.
- Add the Ascend-style `redirectSoftSliceDirs(t)` helper (temp `OperatorLibDir` / `OperatorPodsDir`) to the Hygon
  test file so the on-disk slot scans hit a temp dir; reuse the `slicedPod` / `newSlicedServer` fixture shape.

#### Unit tests
Table-driven; local `darwin`, no hardware. Per-package (date 2026-07-18):
- `pkg/devicemanager/allocator/mthreads`: env encoding — visible-devices join; `MTHREADS_QOS_MEMORY_LIMIT` byte
  conversion; memory-percentage precedence + VRAM cap; missing-memory error; cores default 100; sliced vs
  exclusive/shared/visibility branch isolation.
- `pkg/devicemanager/allocator/hygon` (`vdev.go`): `cuCount` rounding (1 %, fractional, 100 %, zero-cores guard,
  `Cores > 128`); mask low/high-word ordering + boundary CUs 63/64/127; node-wide `vdev_id` uniqueness across cards
  + per-BDF `pipe_id` reuse-across-cards / unique-on-one; restart reconstruction + lowest-hole reuse after dir
  deletion; same-container idempotency; concurrent-goroutine `-race` (distinct ids + disjoint masks); corrupt /
  truncated / duplicate / out-of-range conf fail-closed; an unreadable pod dir fails the scan closed (a walk, not
  `filepath.Glob`, which swallows dir read errors); a `> 128`-CU card and a zero-compute request are rejected
  loudly; atomic publication (a scanner never reads a partial file); `vdev_id` exhaustion at 200 / `pipe_id` at 20 /
  insufficient free CUs / insufficient per-card memory.
- `pkg/devicemanager/allocator/hygon` (branch): exact `vdev.conf` bytes + file mode 0644; mount target
  `/etc/vdev/docker/` + retained `/opt/dtk` → `/opt/hygondriver` + `/opt/hyhal` ro; expected device nodes;
  whole-card marker written (not skipped); whole-card-after-slice, slice-after-whole-card, whole-card on one card of
  a multi-card node; multi-card → one `vdev<i>.conf` per card.

#### Integration tests
- `pkg/deviceplugin` server-level (fake reconciler + temp dirs): MThreads / Hygon `New()` register the Sliced
  server only when `!opts.NoSliced`; the bare `.sliced` resource appears in ListAndWatch; a Sliced Allocate routes
  to the new responder branch; Hygon pod-dir GC over its lifecycle (live / terminating-grace-retained / three-miss
  removal / restart seeding / `RemoveAll` retry) frees the slot on the next scan. Concrete test names added after
  the implementation PR merges.

#### e2e tests
- No real per-vendor isolation e2e — no vendor hardware exists, and the vendor runtime consuming the env /
  `vdev.conf` is a documented post-merge hardware phase. The shipped capability-feedback e2e already covers the
  `.sliced.*` advertisement; extend it only to assert the Hygon `.sliced.cores-percentage` rescale to `cards × 100`
  after the C1 flip (GPU-less-approximated via a fake NFD label + a phantom `Devices` ledger, as the capability
  spec's cases do).

## Alternatives
- **Replicate the HAMi annotation protocol and let the vendor device-plugin do the injection** — rejected:
  contradicts the operator's "NFD counts + self-driven injection" philosophy and would drag in the vendor
  scheduler / webhook / node lock. The operator drives the vendor primitive (env / `vdev.conf`) directly.
- **Implement all four stub vendors in this spec** — rejected per the scope decision: the two stateful vendors
  (MetaX / Cambricon) carry a subdevice lifecycle + reclamation reconciler and are a separate follow-up.
- **In-memory slot counters for Hygon** — rejected: not restart-surviving and desyncs from the on-disk `vdev.conf`
  files the runtime actually reads; the on-disk-derived scan (Ascend's shipped pattern) is authoritative and GC-consistent.
- **Extend the shared `podDirGC` with a vendor teardown hook** — not needed for these two (MThreads is stateless;
  Hygon's only state is the work dir the GC already reclaims). Reserved for the stateful follow-up, which instead
  uses a per-vendor reconciler (the recorded reclamation decision).

## Open Questions
- **MThreads compute-weight scale.** `weight = cores%` maps a 1–100 percent onto the QoS weight (vendor default 1).
  Whether the runtime expects a 1–N relative scale or a 0–100 value must be confirmed on hardware; the mapping may
  need rescaling.
- **Hygon multi-card sliced — resolved (Task 3).** One `vdev<i>.conf` per allocated card (multi-file), each
  independently slotted; the whole-card marker is evaluated per card. Confirm on hardware.
- **Hoisting the on-disk slot-scan helper.** Ascend keeps `lowestFreeVNPUID` in-package; Hygon's slot scan is
  analogous. Whether to generalize a shared `pkg/deviceplugin` helper now or after a third vendor needs it is a
  planning call (the research report's extension question #3) — kept in-package for now.
- **Pre-existing framework issues surfaced by the cross-check (out of scope, noted).** (a) The responder runs
  outside `allocateMutex`, so the shipped Ascend on-disk scan has the same latent concurrency race Hygon now guards
  against; (b) a responder error lands after the durable allocation annotation with no rollback (shared with
  NVIDIA/Ascend); (c) `podDirGC` deletion failures are silent. None are fixed here; they are candidates for a
  framework follow-up.
