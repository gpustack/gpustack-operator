# Spec: Vendor Soft-Slicing Runtime Injection — MetaX (sysfs sGPU) and Cambricon (cnDev sMLU) Allocator Branches

Status: Building
Type: Feature

## Summary
The capability spec `2026-07-16-accelerator-slicing-capability-and-pool-feedback` made all six accelerator
vendors *advertise* a vendor-aware sliced capacity but shipped no real `AllocationMode==Sliced` injection for
four of them, and `2026-07-18-mthreads-hygon-sliced-injection` then delivered the two **stateless /
semi-stateless** vendors (MThreads QoS env, Hygon `vdev.conf`). This spec completes the remaining two — the two
**stateful** vendors — **MetaX** (sysfs `sgpu` subdevices + `METAX_SGPUS` env) and **Cambricon** (cnDev sMLU
profile + instance + device nodes). Both today are stubs: `grep DeviceAllocationModeSliced` hits neither, their
`New()` registers no Sliced server, and their `GetContainerAllocateResponse` only does whole-card passthrough.

Unlike NVIDIA/Ascend/Hygon, whose slice state is a host file the operator wrote (reclaimed by simply deleting
the file), MetaX/Cambricon isolation lives in a **driver / kernel subdevice** the operator must actively
create *and* destroy. The device-plugin responder interface exposes only `GetContainerAllocateResponse` (an
Allocate hook) — **there is no Release callback** — so each vendor gets a **level-based per-vendor reclamation
reconciler** that, fed the same live-pod-UID set the `DevicesReconciler` already broadcasts, destroys the
driver/sysfs subdevices whose pod is gone. An on-disk marker under the pod work dir carries the restart-surviving
subdevice↔pod correlation and slot-derivation ledger (the same shipped pattern as Ascend `npu_info.config` /
Hygon `vdev.conf`), and the reclaim loop additionally reconciles the live driver/sysfs registry so a subdevice
orphaned by a crash between create-and-marker is still reclaimed. Registering each Sliced server also activates
the `.sliced` advertisement the capability spec sized but never wired for these two vendors, so their full
pipeline (advertise → schedule → allocate → inject → reclaim) goes live.

No vendor test hardware is available, so this delivers **build- and unit-test-verifiable** code: the sysfs and
cnDev primitives sit behind a thin, injectable seam (a fake sysfs root; a fake sMLU driver) so every pure
translation, marker, slot-derivation, and reclaim decision is table-tested locally on `darwin`. MetaX needs no
new CGO (pure Go sysfs writes); Cambricon adds **exported cnDev sMLU wrappers** in `binding/cndev` over the
already-generated low-level bindings (cnDev is already linked into the device-manager image by the detector — no
new Dockerfile stage). Real-hardware validation that the vendor runtime consumes the created subdevice /
injected env is a deliberate later phase.

## Motivation
### Goals
- **A sliced MetaX / Cambricon container gets real per-slice isolation**, not a whole-card passthrough: a hard
  VRAM cap and a per-card compute quota derived from the container's `.sliced.cores-percentage` /
  `.sliced.memory-percentage` / `.sliced.memory-mib` request, translated to each vendor's runtime isolation
  primitive.
- **MetaX (stateful, sysfs — landed first):** for a partial slice, ensure the card is in `sgpu` mode, set a
  `sched_class` of **fixed-share** (so `.sliced.cores-percentage` is a hard compute quota, not best-effort),
  write the per-card VRAM quota (MiB) to sysfs `.../sgpu/create`, and inject `METAX_SGPUS`
  (`sgpu=<BDF>#<idx>;compute=<cores%>;vram=<memMiB>;alias=<…>`) plus the `/dev/mxcd` + per-card `/dev/dri/renderD*`
  device nodes. A whole-card slice (`cores% ≥ 100` AND `memMiB ≥ card VRAM`) takes the native whole-card path (no
  sgpu subdevice) but still records occupancy so the on-disk scanner never double-books the card.
- **Cambricon (stateful, cnDev CGO — landed second):** drive cnDev directly — create (or reuse) an sMLU profile
  with `mluQuota = cores%` and `memorySize = memMiB × 1 MiB` (bytes), instantiate an sMLU instance, and inject its
  device nodes (`/dev/cambricon_dev<slot>`, `/dev/cambricon_ipcm<slot>`, the instance `cap_dev*_mi*`) — with a
  `VIRTUAL_DEVICES`-env fallback for `--use-runtime` deployments (sMLU/mim do not support CDI). An sMLU request is
  **single-card** (reject a multi-card sliced allocation, as Ascend rejects multi-NPU).
- **Stateful reclamation via a per-vendor level-based reconciler (no Release hook).** Each vendor spawns a
  reclaim loop, fed the reconciler's broadcast `livePodUIDs` (via `getReconcileNotifier`), that destroys any
  subdevice whose pod has left the live set (after a miss-debounce like `podDirGC`), then removes its marker/dir.
  The loop is level-based and restart-surviving: it re-seeds the used-slot sets from the live driver/sysfs registry
  ∪ the on-disk markers at startup, runs on the broadcast tick **and** a periodic resync ticker (so a dropped tick
  never starves reclamation), takes the shared `allocMu`, and uses a per-pod miss-debounce. To keep a
  create-before-marker crash from ever destroying a *live* slice, a marker-less subdevice is destroyed only when its
  pod is gone; the pod UID is encoded into the subdevice identity (cnDev instance name; MetaX alias) as the
  marker-independent correlation key that makes this safe. (See Reclamation architecture for the exact rules.)
- **Follow the shipped NVIDIA / Ascend / Hygon template exactly:** `New()` gains a `!opts.NoSliced`-gated Sliced
  server → `GetContainerAllocateResponse` branches on `AllocationMode==Sliced` → the branch reuses the public
  `deviceplugin.SlicedCoresPercent` (default 100, overcommit-allowed) / `deviceplugin.SlicedMemoryMib`
  (percentage-preferred, capped, errors if neither set) helpers for `(cores%, memMiB)`. No changes to
  `pkg/nodefeature`, the Pod webhook, Kueue credits, or the `.sliced` sizing in `pkg/deviceplugin`.
- **Compute is a hard per-card partition for both**, so `CoresPercentageOvercommit` stays **false** — which is
  already the zero-value default in both detectors (neither sets it, unlike Hygon which had to flip `true → false`
  in `2026-07-18`), so `.sliced.cores-percentage` already rescales to `cards × 100` via the existing
  overcommit=false branch with **no detector or `node_capacity.go` change**.
- **Success criteria (code-only, no hardware):**
  1. MetaX partial slice: a container requesting `cores% = 60`, `memory-percentage = 50` on a 64 GiB card creates
     one sgpu subdevice with VRAM quota `32768` MiB and `sched_class=fixed-share`, and injects
     `METAX_SGPUS=sgpu=<BDF>#<idx>;compute=60;vram=32768;alias=<…>` plus `/dev/mxcd` + per-card `/dev/dri/renderD*`;
     a second concurrent slice on the same card takes the next free sgpu index; an absent `cores%` defaults to 100.
  2. MetaX whole-card slice (`cores% ≥ 100` AND `memMiB ≥ card VRAM`): takes the native whole-card path (no sgpu
     subdevice created) yet writes an occupancy marker, so the on-disk scanner sees the card taken.
  3. Cambricon: a container requesting `cores% = 25`, `memory-mib = 16384` creates an sMLU profile
     (`mluQuota=25`, `memorySize=16384×1024×1024` bytes) + instance and injects its three device-node classes; a
     multi-card sliced allocation is rejected loudly; an absent `cores%` defaults to 100.
  4. Reclaim (both): after a pod leaves the live-pod set for the per-pod debounce window, its subdevice is destroyed
     and only its specific marker file removed (the dir only when empty); a marker whose subdevice is missing (create
     crash) is cleaned; a marker-less driver/sysfs subdevice is destroyed **only when its embedded pod UID is absent
     from the live set** (a live-UID marker-less subdevice — a create-before-marker crash on a still-reserved pod — is
     left intact); a re-Allocate of the same pod+container reuses the existing subdevice+marker on an exact
     `(card, cores%, memMiB)` match and fails closed on a mismatch. The loop runs on the broadcast tick and a periodic
     resync ticker.
  5. `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./...` and `make lint` are clean; the two allocator packages'
     table tests and the new `binding/cndev` wrapper tests pass under `-race`; **no `make generate`** (no API type
     change; the new cnDev wrappers are hand-written in `library_device.go`, not generated).

### Non-Goals
- **Changing the sliced advertisement / reporting math.** `.sliced`, `.sliced.units`, `.sliced.cores-percentage`,
  `.sliced.memory-*` and the Kueue credit fold-down are owned by the capability spec and unchanged; this spec only
  makes the two vendors *serve* the `.sliced` resource (by registering the Sliced server) and *inject + reclaim*
  real isolation. There is **no** overcommit flip here (both are already `false`), unlike Hygon's C1 in `2026-07-18`.
- **Real-hardware / E2E validation of the vendor runtime consuming the subdevice / env.** No test hardware exists.
  MetaX's exact sysfs paths/schema (`sgpu/create`, `model`, `sched_class`), the injected device-node set, and
  whether the sgpu subdevice supports an alias tag; Cambricon's device-node-vs-`VIRTUAL_DEVICES` injection, the
  cnDev `dynamic-smlu` version prerequisite, and the exact sMLU `SMluSet` field mapping — all MUST be validated on
  real hardware before GA.
- **New Dockerfile xbuild stages / operator-bundled libraries.** MetaX is pure Go sysfs (no library). Cambricon
  reuses the cnDev already linked for detection (dlopen at runtime; the wrappers build on `darwin` without the
  `.so`, matching the existing binding). No `pack/gpustack-operator/Dockerfile` change.
- **Fixing the shared framework gaps** the `2026-07-18` cross-check surfaced (responder runs outside
  `allocateMutex`; a responder error lands after the durable annotation with no rollback; `podDirGC` deletion
  failures are silent). Noted, not fixed here.
- **Changing NVIDIA / Ascend / Hygon / MThreads.** Their branches are untouched.
- **Per-card `cores-percentage` accounting (K1) and webhook `.sliced > 1` enforcement (K2).** Both are shared,
  capability-spec / Pod-webhook–owned follow-ups (see Open Questions), not this spec.

## Proposal
Add a real sliced injection branch plus a level-based reclamation reconciler to the MetaX and Cambricon
allocators, using the established per-vendor template and the shipped shared plumbing, so a sliced pool of either
vendor is functional end-to-end on real hardware once validated.

### Vendor facts (grounding the branches)

**MetaX sGPU.** Compute key `vcore` = **% (1-100)**; memory `vmemory` default unit **Gi** → internally MiB; a whole
card is **65536 MiB**. Isolation is executed by operating **sysfs** and injecting **`METAX_SGPUS`** (there is **no**
`METAX_VISIBLE_DEVICES`). The vendor plugin splits on `Compute == 100 → useNativeCard`: a whole card takes
`model=native` and mounts `/dev/mxcd` + `/dev/dri/renderD*`; otherwise the sGPU path sets `model=sgpu`, writes
`sched_class` (best-effort=0 / fixed-share=1 / burst-share=2), writes the VRAM quota (MiB) to sysfs
`/sys/bus/pci/devices/<BDF>/sgpu/create`, and injects `METAX_SGPUS` (`sgpu=<BDF>#<idx>;compute;vram;alias`). Each
card hosts **≤ 16 subdevices** (`DefaultDevCnt=16`); default compute quota 100 / memory quota 65536 MiB; the plugin
writes card capacity to node annotation `metax-tech.com/node-gpu-devices` and runs a **90s GC** for orphan
subdevices. QoS policies: `sgpu-qos-policy` best-effort / fixed-share (hard quota) / burst-share (quota + idle
burst); **the `sched_class` of in-use subdevices on one card must be consistent — switching it while a subdevice is
in use is rejected**; `sgpu-app-class` (online/offline preemption) requires single-card.

**Cambricon sMLU.** Compute key `vcore` = **% (0-100)** (HAMi Fit caps at 100; `= 100` is whole-card exclusive, a
busy card is refused); memory `vmemory` is a HAMi-internal **256 MiB/unit** accounting figure that **cancels out**
(request and node total both scale by 256, and the annotation carries a unit *count*, not bytes) — the physical
granularity is the device-plugin's `memUnit` (default card-VRAM/100). Isolation is a **cnDev sMLU profile**
(`mluQuota = Vcore`, `memorySize = Vmemory × memUnit × 1 MiB`) instantiated into a real subdevice; the final mount
is **3 device-node classes** `/dev/cambricon_dev<slot>`, `/dev/cambricon_ipcm<slot>`, the instance
`cap_dev<N>_mi<X>` — or, under `--use-runtime`, a `VIRTUAL_DEVICES` env (sMLU/mim do **not** support CDI). A single
sMLU request is **1 pod / 1 container / 1 card**; each card has 100 time slices. The vendor plugin serializes with a
2-minute node lock `cambricon.com/dsmlu.lock`.
> **Unit clarification (verified):** the `× 256` is pure HAMi-internal accounting that self-cancels — it is **not** a
> runtime constraint and does not enter this design. The operator drives cnDev directly with clean units: `memorySize
> = memMiB × 1 MiB` (bytes) and `mluQuota = cores%` (percent), bypassing the 256/`memUnit` indirection entirely, so
> no `--min-dsmlu-unit` alignment is needed.

**Shared translation.** The two shipped helpers already produce the exact inputs — `SlicedCoresPercent(ctr,
coresRes)` (default 100) and `SlicedMemoryMib(ctr, memPctRes, memMibRes, cardVRAMMib)` (percentage-preferred,
capped at the card VRAM, errors if neither set) — and **both vendors use the compute percent directly** (MetaX
`METAX_SGPUS compute` / sysfs; Cambricon cnDev `mluQuota`), so, unlike Hygon, there is **no CU-count conversion**
and `group.Cores` is not needed. No per-vendor memory/percentage math is re-derived.

| Vendor | Compute key → primitive | Memory key → primitive | State | Isolation executed by |
|---|---|---|---|---|
| MetaX | `cores%` → sysfs `sched_class=fixed-share` + `METAX_SGPUS compute=<%>` (hard) | `memMiB` → sysfs `.../sgpu/create` VRAM quota + `METAX_SGPUS vram=<MiB>` | sysfs sgpu subdevice + per-card idx pool | host MetaX driver (sgpu mode) + MXMACA runtime reading the subdevice |
| Cambricon | `cores%` → cnDev sMLU `mluQuota` (hard) | `memMiB×1Mi` → cnDev sMLU `memorySize` bytes | cnDev sMLU profile + instance (driver) | host cnDev / Cambricon driver (dynamic-smlu) |

### Reclamation architecture (marker + per-vendor reconciler)
The responder interface has only `GetContainerAllocateResponse`; there is **no Release callback**. For file-state
vendors (Ascend/Hygon) `podDirGC` suffices because deleting the host file *is* the reclamation. MetaX/Cambricon
state is a driver/kernel subdevice, so reclamation must be active. The recorded decision (from `2026-07-18`) is a
**per-vendor reconciler that destroys the driver/sysfs subdevices whose pod is gone — not an extension of
`podDirGC`**. The mechanism below is **hardened by the plan-stage Codex cross-check** (which found that a naive
"marker-less subdevice → destroy" rule would destroy a *live* allocation after a create-before-marker crash, that
marker-only slot derivation undercounts real driver objects, that removing a pod *dir* can orphan a sibling
container, and that a lossy broadcast channel can starve reclamation — all folded in here):

- **Allocate writes an on-disk marker** under `PodWorkDir(pod.UID, ctr.Name)` (e.g. `metax-sgpu.json` /
  `cambricon-smlu.json`) recording the correlation + slot ledger: `{podUID, container, cardBDF|index,
  subdeviceIdx | sMLU instance name+handle, cores%, memMiB}`. Published via temp-file + atomic rename (a scanner
  never reads a partial marker). It is the restart-surviving analogue of Ascend's `npu_info.config` / Hygon's
  `vdev.conf`. **The subdevice identity ALSO encodes the pod UID** where the primitive allows (cnDev instance name;
  MetaX alias) — a second, marker-independent correlation key that makes the crash-orphan rule below safe.
- **Slot derivation reconciles the live driver/sysfs registry ∪ the on-disk markers** — never markers alone. A
  driver object with no marker (a crash orphan) still occupies its index, so the lowest-free per-card index is
  computed against the *union*; the ≤ 16-subdevice/card cap counts **actual driver objects**, not markers. Runs
  under `allocMu`, **fail-closed scoped to the affected card**: a corrupt marker fails that one card's derivation
  (loud reschedule), never all of the vendor's allocations.
- **A per-vendor reclaim loop** (spawned in `aggregated.Start`) is driven by **both** a `getReconcileNotifier(
  Manufacturer, Sliced)` subscription (the broadcast `livePodUIDs`, including grace-period terminating pods) **and
  a periodic resync ticker + a startup full scan** — because the broadcast channel is lossy (buffered, non-blocking
  send drops a tick when full), the ticker guarantees reclamation eventually runs regardless of dropped ticks
  (closing the starvation the cross-check flagged). It takes **the same `allocMu`** for its scan → destroy critical
  section (so it never observes or races an in-flight Allocate's create-before-marker window), and uses a
  **per-pod** liveness decision with a `podDirGC`-style 3-consecutive-miss debounce (miss counted only on an
  observed-absent snapshot; reset on any observed-live snapshot; a dropped tick neither increments nor resets), so a
  transient list gap never partially reclaims a multi-container pod:
  - a marker whose `podUID ∉ live` (debounced) → **destroy the subdevice** (sysfs remove /
    `cndevDestroySMluInstanceByName`), then **remove only that marker file** — remove the pod dir only when empty
    (never delete a sibling container's live marker);
  - a marker whose subdevice is **missing** from the live registry (create crashed before the marker, or external
    teardown) → clean that marker file only;
  - a live driver/sysfs subdevice with **no** marker → destroy it **only if its embedded pod UID is absent from the
    live set** (a genuine orphan); a marker-less subdevice whose embedded UID is still live (a create-before-marker
    crash on a still-reserved pod) is **left intact**, not destroyed. Where the primitive cannot carry a UID (a
    MetaX subdevice with no readable alias), this rule is conservative — it does not auto-destroy a marker-less
    subdevice on a card that still hosts a live-reserved pod, trading a rare transient leak (reclaimed once the pod
    is gone) against never killing a live slice.
- **Idempotency + integrity.** A re-Allocate of the same `PodWorkDir` reuses the existing marker + subdevice **only
  on an exact `(card, cores%, memMiB)` match**; a mismatch (pod resource requests are immutable, so this signals
  corruption or a bug) **fails closed** rather than mutating a live slice. Cambricon reuses a profile only on an
  exact `(mluQuota, memorySize)` match and never destroys a profile another instance still references. A MetaX
  `sched_class` is written only when the card has no incompatible in-use operator subdevice; a live card is never
  mutated in place.
- **Known residual (shared H6 framework strand, documented not fixed).** The framework persists the pod reservation
  *before* the responder runs, and a kubelet retry skips already-reserved pods — so a crash between subdevice-create
  and marker-write strands a charged-but-wedged pod until it is deleted (same as NVIDIA/Ascend/Hygon). `allocMu` and
  the softened rule (c) ensure this is at worst a stranded/leaked slice reclaimed on pod deletion — never a
  destroyed live slice. Closing the reservation↔responder transaction is a framework follow-up, out of scope here.

### User Stories
#### Story 1 — MetaX slice gets a hard compute + VRAM partition
As a **workload user**, when I request a sliced MetaX device with a compute and memory percentage, I want an sgpu
subdevice created with `sched_class=fixed-share`, a hard VRAM quota, and `METAX_SGPUS` injected, so that my slice
is compute- and memory-partitioned from co-located slices (not best-effort).

#### Story 2 — MetaX whole-card slice still records occupancy
As the **platform**, when a MetaX sliced request resolves to a whole card (100% compute and full VRAM), I want the
native whole-card path taken (no sgpu subdevice) yet an occupancy marker still written, so that the on-disk scanner
sees the card as taken and never double-books it on a multi-card node.

#### Story 3 — Cambricon slice gets a cnDev sMLU instance
As a **workload user**, when I request a sliced Cambricon device, I want a cnDev sMLU profile + instance created
with my `cores%` compute quota and `memMiB` VRAM cap and its device nodes injected, so that my slice is
driver-level isolated from co-located slices.

#### Story 4 — A single-card guard for Cambricon sMLU
As a **cluster operator**, when a Cambricon sliced request would span more than one card, I want it rejected loudly
(as Ascend rejects multi-NPU), so that an sMLU slice never silently isolates only the first card while exposing the
rest.

#### Story 5 — A dead slice frees its subdevice automatically (no Release hook)
As the **platform**, when a pod holding a MetaX/Cambricon slice disappears, I want its driver/sysfs subdevice
actively destroyed and its marker reclaimed by the level-based per-vendor reconciler, so that subdevices never leak
(exhausting the ≤16-per-card pool) across restarts and no Release callback is needed.

#### Story 6 — The translation + lifecycle is verifiable without hardware
As a **maintainer**, I want the sysfs / cnDev primitives behind an injectable seam (a fake sysfs root; a fake sMLU
driver) and the marker / slot-derivation / reclaim decisions implemented as pure, table-tested functions, so that
the branch and its reclamation are correct and reviewable before any hardware exists.

### Core Features & Acceptance Criteria
| # | Feature | Acceptance |
|---|---------|-----------|
| F1 | MetaX Sliced server + sGPU branch | `metax/deviceplugin.go`: `New()` registers a `!opts.NoSliced` Sliced server; `GetContainerAllocateResponse` gets an `AllocationMode==Sliced` branch that (partial) creates an sgpu subdevice (`sched_class=fixed-share`, VRAM quota MiB) and injects `METAX_SGPUS` + `/dev/mxcd` + per-card `/dev/dri/renderD*`; non-sliced modes unchanged. |
| F2 | MetaX whole-card occupancy | `cores% ≥ 100` AND `memMiB ≥ card VRAM` → native whole-card path (no sgpu subdevice) + an occupancy marker (never a config-less hole the scanner misses). |
| F3 | MetaX sysfs seam + slot derivation | The sysfs ops (`model`, `sched_class`, `sgpu/create`, subdevice enumerate/remove) sit behind an interface with a real sysfs impl + a fake root; per-card sgpu-index derivation is lowest-free from the on-disk markers (level-based, restart-surviving), under a package mutex, fail-closed. |
| F4 | Cambricon exported cnDev sMLU wrappers | `binding/cndev/library_device.go`: hand-written exported wrappers over the generated low-level calls — create/destroy sMLU profile, create/destroy sMLU instance (by handle + by name), list all sMLU instances — each with the `so.Lookup` guard so they build/test on `darwin` without the `.so`. |
| F5 | Cambricon Sliced server + sMLU branch | `cambricon/deviceplugin.go`: `New()` registers a `!opts.NoSliced` Sliced server; the Sliced branch creates/reuses a profile (`mluQuota=cores%`, `memorySize=memMiB×1Mi` bytes) + instance and injects its device nodes (with a `VIRTUAL_DEVICES` fallback); a multi-card sliced allocation is rejected. |
| F6 | Per-vendor reclamation reconciler | Each vendor spawns a level-based reclaim loop driven by the broadcast `livePodUIDs` **and** a periodic resync ticker + startup scan (so a dropped notifier tick never starves reclamation); it takes the shared `allocMu`, uses a **per-pod** 3-miss debounce, and reconciles marker ↔ live driver/sysfs registry: destroys a subdevice whose pod is gone; cleans a subdevice-less marker; destroys a marker-less subdevice **only if its embedded pod UID is absent from the live set** (never a live-reserved crash-in-progress); removes only the specific marker file (dir only when empty). Slot derivation and the ≤16 cap count the **registry ∪ markers**. No Release hook; not an extension of `podDirGC`. |
| F7 | Code-only tests | Table-driven tests for the pure translation / marker / slot / reclaim functions (fake sysfs root; fake sMLU driver; temp `OperatorPodsDir`), `-race` allocMu↔reclaim, fail-closed parsing (scoped per-card), idempotent-reuse + changed-param fail-closed, whole-card marker, single-card reject, and the crash-window orderings. `go build ./...` + `make lint` clean; no `make generate`. |

### Notes / Constraints / Caveats
- Go + controller-runtime; local `darwin/arm64`, `CGO_ENABLED=1`, `GODEBUG=gotypesalias=0`. Follow the Go /
  Kubernetes / testing conventions in `CLAUDE.md`. No API type change → **no `make generate`**, no image build.
- **Enabling the Sliced server is a behavior change for these two vendors:** it activates the `.sliced` resource
  (sized `cards × maxSlices` by `pkg/deviceplugin`; both detectors already set `LogicalSliced{MaxSize:16}`) and the
  presence-gated `.sliced.*` node capacities, so a node with MetaX/Cambricon now schedules and admits sliced
  workloads through the new branch. This closes the gap where the capability spec sized `.sliced` but no Sliced
  server existed to serve it — the same activation note the `2026-07-18` spec made for MThreads/Hygon.
- **Device-manager host access is already sufficient:** the DaemonSet is `privileged: true` and mounts `/sys` and
  `/var/lib/gpustack` (host), so MetaX sysfs writes, Cambricon cnDev calls, and the on-disk markers all work with no
  chart change. The detectors already read `/sys` (MetaX `getPhysicalIndexes`) and call cnDev, so the driver access
  path is proven.
- **Both compute dimensions are hard per-card partitions**, so `CoresPercentageOvercommit` is `false` — already the
  zero-value default in both detectors (no flip, no detector change).
- **cross-check status.** A plan-stage **Codex** read-only design red-team ran and materially hardened the
  reclamation design (folded above + into Risks): the softened rule (c), registry-∪-markers slot derivation, remove
  only the specific marker file, per-pod debounce, the periodic resync ticker, exact-match idempotency, and the
  documented H6 crash-window strand. (A Kimi run was attempted first but hung at session-init and was cancelled,
  producing nothing — so the plan-stage second voice is Codex's.) The **`/my-build` phase should run the adversarial
  cross-check over the real diff** — Codex `/codex:review` (or `/codex:adversarial-review`) and a Kimi retry — on
  (a) the reclaim loop under crash/restart + concurrent Allocate, and (b) the cnDev wrapper signatures + `SMluSet`
  field mapping, per the `crosscheck` decision procedure.

### Boundaries
- **Always:** follow the NVIDIA/Ascend/Hygon allocator template; reuse `SlicedCoresPercent` / `SlicedMemoryMib`;
  keep marker + slot derivation on-disk-derived and level-based (restart-surviving); put the sysfs / cnDev
  primitives behind an injectable seam so the logic is table-tested without hardware; actively destroy subdevices in
  a per-vendor reconciler fed the broadcast `livePodUIDs` **plus a periodic resync ticker**, deriving slots and the
  ≤16 cap from the live registry ∪ markers; run `make lint` + the two allocator packages + `binding/cndev`
  + `pkg/deviceplugin` tests + a whole-module `darwin` build; land MetaX before Cambricon.
- **Ask first:** adding any Dockerfile xbuild stage or operator-bundled library (should not be needed); changing the
  MetaX default `sched_class` away from fixed-share; changing the shared `podDirGC` signature or the `pkg/deviceplugin`
  `.sliced` sizing; regenerating `binding/cndev` (the wrappers are hand-written in `library_device.go`); restricting
  Cambricon sliced to anything other than single-card.
- **Never:** implement any behavior for the already-shipped vendors here; use in-memory (non-restart-surviving) slot
  counters; **destroy a marker-less subdevice whose pod is still live** (the softened rule (c) — the load-bearing
  correctness invariant); leak a subdevice when its pod is gone; remove a pod dir that still holds a sibling
  container's marker; silently expose a whole card to a *partial* sliced request without creating the isolation
  subdevice; claim real-hardware validation (none exists).

### Risks and Mitigations
- **No Release hook → a subdevice leaks after its pod is gone** → Mitigation: the per-vendor level-based reclaim loop
  (F6) is the primary reclamation; it re-seeds from markers + the live registry at startup and runs on **both** the
  broadcast tick **and** a periodic resync ticker, so a leak self-heals within a bounded interval. Proven by unit
  tests over a fake driver/sysfs + temp pods dir.
- **(Codex) A naive "marker-less subdevice → destroy" rule destroys a LIVE slice after a create-before-marker crash**
  → the subdevice exists, the pod is still live & reserved, but no marker landed → the orphan rule would kill a live
  allocation. **Mitigation:** rule (c) destroys a marker-less subdevice **only if its embedded pod UID is absent from
  the live set**; a marker-less subdevice whose embedded UID is still live is left intact. Where the primitive cannot
  carry a UID, the rule is conservative (no auto-destroy on a card hosting a live-reserved pod). This is the single
  most load-bearing correctness fix from the cross-check.
- **(Codex) Marker-only slot derivation undercounts real driver objects after a crash-orphan → double-book** → a
  driver object with no marker makes its index look free. **Mitigation:** derive the lowest-free index and enforce the
  ≤16 cap against the **live registry ∪ markers**, never markers alone.
- **(Codex) Removing the pod DIR while reclaiming one marker double-frees a sibling container** → a pod with markers
  for containers `a` and `b`; reclaiming `a` by deleting the pod dir drops `b`'s live marker → rule (c) then destroys
  `b`'s subdevice. **Mitigation:** remove only the specific marker *file*; remove the pod dir only when empty.
- **(Codex) Lossy broadcast channel starves reclamation under churn** → a full buffered channel drops ticks silently
  and the loop scans only on a received tick → a dead slice can persist indefinitely. **Mitigation:** a periodic
  resync ticker + startup scan drives the loop independent of the broadcast (chosen over a controller-runtime
  workqueue for minimal blast radius; the broadcast becomes a latency optimization, not the sole guarantee).
- **Crash between subdevice-create and marker-write orphans state** → Mitigation: the reclaim loop reconciles *both*
  directions (subdevice-less marker cleaned; marker-less subdevice destroyed **only if its embedded UID is dead**),
  and marker publication is atomic (temp + rename), so no half-state is acted on unsafely. The residual — a
  charged-but-wedged pod when the create-before-marker crash leaves a still-reserved pod (the H6 framework strand) —
  is documented, not fixed here; it never becomes a *destroyed* live slice.
- **Concurrent Allocates / Allocate-vs-reclaim race** → Mitigation: a package-level `allocMu` guards scan → validate →
  create → write in Allocate **and** the scan → destroy critical section in the reclaim loop (both take it), so
  reclaim never observes an in-flight create-before-marker window; `-race`-tested. The responder runs outside the
  reconciler `allocateMutex`, so this local guard is required. Note `allocMu` is not a transaction with the framework
  reservation and does not survive a crash (hence the softened rule (c) above).
- **MetaX sysfs paths/schema are single-source (report/vendor doc, not code-verified here)** → Risk: exact
  `sgpu/create` / `model` / `sched_class` layout, the injected device-node set, or alias-tag support may differ.
  Mitigation: code to the documented schema behind the injectable seam, unit-test the encoding, and gate GA on
  hardware validation.
- **Cambricon cnDev sMLU wrapper mapping is CGO + hardware-unverifiable** → Risk: `SMluSet` field semantics
  (`mluQuota` unit, `memorySize` bytes-vs-units) may differ from the report's reading. Mitigation: hand-write the
  wrappers over the generated bindings with `so.Lookup` guards (build/test on `darwin`), isolate the `(cores%,
  memMiB) → SMluSet` mapping in a pure function, Codex/Kimi cross-check the signatures, and gate GA on hardware.
- **MetaX `sched_class` conflict on a shared card** → Risk: a card with an in-use subdevice of a different
  `sched_class` rejects a new one, or writing it mutates live slices' scheduling. Mitigation: the operator always uses
  `fixed-share`; it writes `sched_class` **only when the card has no incompatible in-use operator subdevice** and
  never mutates a card with live slices; a create that still fails (a foreign subdevice) fails closed and reschedules.
- **Cambricon multi-card sliced** → Risk: sMLU is 1-pod/1-ctr/1-card. Mitigation: reject a multi-card sliced
  allocation loudly (the Ascend single-NPU pattern).
- **(Codex) Cambricon instance-name overflow / collision** → a k8s UID is 36 chars and a container name up to 63, so
  `UID+sep+container` can exceed the cnDev 100-byte `InstanceName` buffer (incl. NUL); truncation could collide two
  allocations onto one name. **Mitigation:** encode `<prefix>:<podUID>:<shortHash(container)>` (bounded length), and
  keep the full pod↔container↔instance map in the marker; the instance name is only a unique, decodable correlation key.
- **(Codex) Cambricon profile reuse/destroy identity** → reusing a profile with a non-identical quota violates the
  requested isolation; destroying a profile another instance still references breaks that instance. **Mitigation:**
  reuse a profile only on an exact `(mluQuota, memorySize)` match; never destroy a profile with a live referencing
  instance (verify cnDev refcount semantics on hardware).
- **(Codex) Corrupt-marker fail-closed becomes a per-card capacity outage** → a single malformed/stale marker fails
  slot derivation. **Mitigation:** scope the fail-closed to the **affected card** only (loud reschedule onto another
  card), never fail all vendor allocations node-wide.
- **(Codex) Per-slice debounce can partially reclaim a multi-container pod** → per-slice miss counters can hit the
  threshold at different times on a transient list gap. **Mitigation:** make the liveness decision **per-pod** (the
  pod UID is present/absent), with per-slice cleanup progress under that stable decision.
- **(Codex) API liveness ≠ container stopped (force-delete / node partition)** → a force-deleted or partitioned pod can
  leave the live set while its container still runs → reclaim could destroy a running slice's isolation. **Mitigation:**
  the per-pod 3-miss debounce narrows the window; this mirrors the vendor plugins' own 30s/90s GC (same property);
  accepted residual, documented. A termination-timeout policy for stuck-terminating pods is a follow-up.
- **K1 — `overcommit=false` + default `cores% = 100` lets a slice monopolize a card's compute** (shared with Hygon) →
  a memory-only slice leaves `cores-percentage` at 100, reserving the whole card's compute; because
  `cores-percentage` is accounted node-wide (`cards × 100`), two default-cores slices can co-locate and the second's
  subdevice create fails closed *after* the durable annotation → a wedged pod + leaked `.sliced.units` until deletion.
  **No in-scope fix** (per-card `cores` accounting / a non-100 default lives in the capability spec / Pod webhook).
  Mitigation: documented open question + operator caveat (set `cores-percentage` explicitly); the failure is loud.
- **K2 — a `.sliced > 1` request on one card under-serves while over-charging** (shared) → both vendors emit one
  subdevice per physical card; only NVIDIA meaningfully serves `.sliced > 1`. Control belongs in the Pod webhook
  (reject `.sliced > 1` for non-NVIDIA vendors); documented, a follow-up.
- **No test hardware** → all verification is build + unit-test + lint; vendor-runtime consumption is a documented
  post-merge hardware phase, not a silent gap.

## Design Details
### Commands
```bash
# No API change → no `make generate`, no image build. Whole-module build smoke on darwin (CGO detectors + cnDev):
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./...
# Targeted package tests (allocators + new cnDev wrappers + server-level integration):
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race \
  ./pkg/devicemanager/allocator/metax/... ./pkg/devicemanager/allocator/cambricon/... \
  ./binding/cndev/... ./pkg/deviceplugin/...
# Lint (also fails if the tree is left dirty; whole-module golangci-lint — use a long timeout / background it):
make lint
```
### Project Structure (files in scope)
```
# MetaX (landed first — pure Go sysfs, no new CGO)
pkg/devicemanager/allocator/metax/deviceplugin.go        # F1/F2 Sliced server + sGPU branch + whole-card marker + reclaim wiring
pkg/devicemanager/allocator/metax/sgpu.go                # F3/F6 NEW: sysfs seam (model/sched_class/create/remove), METAX_SGPUS encode, marker + slot derivation + reclaim (package mutex, fail-closed)
pkg/devicemanager/allocator/metax/deviceplugin_test.go   # F7 NEW: branch table tests (fake sysfs root, temp OperatorPodsDir)
pkg/devicemanager/allocator/metax/sgpu_test.go           # F7 NEW: encode/marker/slot/reclaim/-race/fail-closed tests

# Cambricon (landed second — adds cnDev CGO wrappers)
binding/cndev/library_device.go                          # F4 add exported sMLU wrappers over generated low-level calls
pkg/devicemanager/allocator/cambricon/deviceplugin.go    # F5 Sliced server + sMLU branch + single-card reject + reclaim wiring
pkg/devicemanager/allocator/cambricon/smlu.go            # F5/F6 NEW: (cores%,memMiB)→quota mapping, seam + profile/instance reuse-destroy, marker + reclaim (cnDev-free core)
pkg/devicemanager/allocator/cambricon/smlu_driver_linux.go # F4/F5 NEW (Task 5, linux-tagged): real cndevSMLUDriver over the wrappers (+ a !linux stub)
pkg/devicemanager/allocator/cambricon/deviceplugin_test.go # F7 NEW
pkg/devicemanager/allocator/cambricon/smlu_test.go       # F7 NEW: mapping/marker/reclaim/fail-closed (fake sMLU driver)
```
### Code Style
```go
// MetaX sliced branch: compute is a hard fixed-share quota; memory is a hard VRAM cap. Both come
// straight from the shared helpers (no CU conversion — the percent is used directly).
coresPct := deviceplugin.SlicedCoresPercent(ctr,
    nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer)) // default 100
memMib, err := deviceplugin.SlicedMemoryMib(ctr,
    nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer),
    nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer),
    int64(group.Memory))
if err != nil {
    return nil, fmt.Errorf("derive sliced memory limit: %w", err)
}
// Whole-card slice: native path, no sgpu subdevice, but still record occupancy.
if coresPct >= 100 && memMib >= int64(group.Memory) {
    return s.wholeCardResponse(pod, ctr, accel /* writes an occupancy marker */)
}

// sysfs / cnDev sit behind an injectable seam so the logic is table-tested without hardware:
//   type sgpuManager interface { EnsureMode(bdf, mode string) error; SetSchedClass(bdf string, c schedClass) error
//                                Create(bdf string, vramMiB int64) (idx int, err error); Remove(bdf string, idx int) error
//                                List(bdf string) ([]sgpuSubdevice, error) }
// The real impl writes /sys/bus/pci/devices/<BDF>/sgpu/*; the test impl uses a fake root.
```
### Implementation Plan
**Dependency graph.** MetaX (Tasks 1–2) lands the stateful template + the per-vendor reclaim reconciler on the
CGO-free vendor, fully fake-testable, before Cambricon (Tasks 3–5) layers the cnDev wrapper surface on the proven
lifecycle. Task 1 is a de-risk spike (the sysfs seam + marker/slot/reclaim core — the riskiest surface, and the one
the Codex cross-check hardened); Task 2 wires it into the responder + reclaim loop. Task 3 (cnDev wrappers) and
Task 4 (sMLU lifecycle core) are Cambricon de-risk spikes; Task 5 wires them. Each task leaves the tree
building/linting/green; checkpoints after **Task 2** (MetaX complete) and **Task 5** (Cambricon complete). All
verification is local `darwin` (`GODEBUG=gotypesalias=0 CGO_ENABLED=1`); no image build, no `make generate`.

- [x] **Task 1 (MetaX de-risk spike) — sGPU translation + lifecycle core (`metax/sgpu.go`).** The `sgpuManager` seam
  (`EnsureModel`/`SetSchedClass`/`Create(bdf,vramMiB)→idx`/`Remove(bdf,idx)`/`List(bdf)→[]sgpuSubdevice`) with a real
  sysfs impl + a fake root; a pure `METAX_SGPUS` encoder; marker render/parse (fail-closed, temp+atomic-rename);
  per-card lowest-free sgpu-idx derivation over the **live registry ∪ markers** under a package `allocMu`; and the
  bidirectional `reclaim(livePodUIDs, mgr)` — per-pod 3-miss debounce, **taking `allocMu`**, with the softened
  rule (c) (destroy a marker-less subdevice only if its embedded UID is dead). **Accept:** fake-root table tests —
  encode; marker round-trip + corrupt→error (scoped to the card); lowest-free idx (2 same-card / different-card
  reset / registry object with no marker still occupies its idx); idempotent reuse + changed-param→fail-closed;
  whole-card path writes marker but creates no subdevice; reclaim destroys dead-pod subdevice after 3 misses, cleans
  subdevice-less marker, destroys marker-less-subdevice **only when its UID is dead** (leaves a live-UID one),
  removes only the specific marker file (never a sibling's), re-seeds registry∪markers at startup, resync-ticker
  drives a reclaim even with zero broadcast ticks; `-race` concurrent Allocate+reclaim never double-frees.
  **Verify:** `go test -race ./pkg/devicemanager/allocator/metax/... && make lint`.
- [x] **Task 2 — MetaX Sliced responder branch + reclaim wiring (`metax/deviceplugin.go`).** `New()` registers a
  `!opts.NoSliced` Sliced server; `aggregated.Start` spawns the reclaim loop (broadcast subscription via
  `getReconcileNotifier(Manufacturer, Sliced)` + periodic resync ticker + startup scan); the `AllocationMode==Sliced`
  branch computes `(cores%, memMiB)`, takes the whole-card vs partial path, returns `METAX_SGPUS` + `/dev/mxcd` +
  per-card `/dev/dri/renderD*`. Name the previously-ignored `pod`/`ctr` params. **Accept:** branch table tests
  (mirror Ascend) — partial slice env+devices+marker; whole-card marker/no-subdevice; default cores%=100; no-memory
  →error; sliced vs exclusive/shared/visibility isolation; a fake reconciler tick + a resync tick each reclaim a
  dead-pod slice. **Verify:** `go test -race ./pkg/devicemanager/allocator/metax/... ./pkg/deviceplugin/... && make
  lint`. **Checkpoint:** `go build ./...` + `make lint` clean; MetaX pipeline green.
- [x] **Task 3 (Cambricon de-risk spike) — exported cnDev sMLU wrappers (`binding/cndev/library_device.go`).**
  Hand-write, each `so.Lookup`-guarded (build/test on darwin without the `.so`): `SetSMLUMode(enabled bool)`/`GetSMLUMode`,
  `CreateSMluProfile(SMluSet)→(profileID int32, Return)`, `DestroySMluProfile(profileID)`,
  `CreateSMluInstance(profileID uint32, name string)→Return`, `DestroySMluInstanceByName(name)`,
  `GetAllSMluInstanceInfo()→([]SMluInfo, Return)`. The raw `cndevMluInstance` handle is **not** returned from
  `CreateSMluInstance`: it is an unexported cnDev type (unusable across packages, trips revive `unexported-return`)
  and the instance is addressed by name for every downstream op (destroy-by-name, list). **Accept:** builds on
  darwin; a nil-`Lookup` guard returns `ERROR_FUNCTION_NOT_FOUND`; guard-path unit test; no `make generate`
  (hand-written, not `zz_generated`). **Verify:** `go build ./binding/cndev/... && go test ./binding/cndev/... && make lint`.
- [x] **Task 4 (Cambricon de-risk spike) — sMLU lifecycle core (`cambricon/smlu.go`).** The `smluDriver` seam
  (`EnsureSMLUMode`, `CreateProfile`/`DestroyProfile`, `CreateInstance`/`DestroyInstance`, `ListInstances()→[]instance`)
  + a fake driver; the pure `(cores%, memMiB)→(mluQuota, memorySize)` mapping (`mluQuota=cores%`, `memorySize=memMiB<<20`
  bytes); bounded instance-name encoding (`<prefix>:<podUID>:<shortHash(ctr)>`, ≤100 B incl. NUL) + marker + `reclaim`
  (list instances via driver, decode UID, destroy dead-UID orphans; marker ↔ registry both directions; profile reuse
  only on exact `(quota,mem)` match; never destroy a shared profile; under `allocMu`). **Refinements from the plan:**
  (a) the seam is **thin create/destroy/list over the Task-3 wrappers** rather than a combined `CreateProfileInstance`,
  so profile find-or-create + refcount live *above* the seam (in `reserveInstance`/`reclaim`) and are testable with the
  fake — a combined seam would push reuse into the driver, testable only against hardware; (b) the mapping returns the
  two quota **values** (not a `cndev.SMluSet`) so `smlu.go` imports no cnDev cgo; (c) the **real `cndevSMLUDriver` moves
  to Task 5** (`smlu_driver_linux.go`, linux-tagged, + a darwin stub) — it is dead code until the responder wires it
  (would trip `unused`/`unparam`), and linking cnDev into the Task-4 test binary aborts at **dyld load on darwin**
  (macOS chained fixups eagerly bind the flat-namespace `_cndev*` symbols the `.so`-less host cannot resolve), so the
  core stays cnDev-free and fully `darwin` build+test-able. **Accept:** fake-driver table tests — mapping; name
  encode/decode round-trip incl. max UID + 63-char container (fits, no collision); marker fail-closed; reclaim destroys
  dead-pod / instance-less-marker / marker-less-dead-UID and leaves marker-less-live-UID + foreign names; idempotent
  reuse + changed-param→fail-closed + reuse-missing-instance→fail-closed; exact-match profile reuse (per card) + never
  destroy a shared profile; create-failure rolls back the profile; `-race`. **Verify:**
  `go test -race ./pkg/devicemanager/allocator/cambricon/... && make lint` (clean; cambricon coverage 66.5%, climbs
  past the ≥70% target once Task 5 adds the responder tests).
- [ ] **Task 5 — Cambricon real cnDev driver + Sliced responder branch + reclaim wiring (`cambricon/deviceplugin.go`,
  `cambricon/smlu_driver_linux.go`).** Introduce the real `cndevSMLUDriver` over the Task-3 wrappers in
  `smlu_driver_linux.go` (linux build tag, so the cnDev cgo never reaches the `darwin` test binary) with a `!linux`
  stub, both behind a `newSMLUDriver()` platform seam. Then `New()` + `Start` as Task 2; the Sliced branch **rejects
  multi-card** (Ascend pattern), computes `(cores%, memMiB)`, creates/reuses the profile+instance via the Task-4 core,
  and injects the device nodes (`/dev/cambricon_dev<slot>`, `_ipcm<slot>`, the instance `DevNodeName`) with a
  `VIRTUAL_DEVICES`-env fallback flag. **Accept:** single-card slice creates
  instance + device nodes + marker; multi-card→error; default cores%=100; no-memory→error; sliced vs other modes
  isolated; reclaim wired. **Verify:** `go test -race ./pkg/devicemanager/allocator/cambricon/... ./pkg/deviceplugin/...
  ./binding/cndev/... && make lint`. **Checkpoint:** whole-module `go build ./...` + `make lint` clean; all in-scope
  package tests green; no `make generate`.
- *Cross-check (per `crosscheck`):* at `/my-build`, run the adversarial review over the real diff — Codex
  `/codex:review` (or `/codex:adversarial-review`) on the reclaim loop (crash/restart + concurrent Allocate) and a
  Kimi retry on the cnDev wrapper signatures + `SMluSet` mapping. Real-hardware validation of the injected env /
  subdevice is a documented post-merge phase, not a checkpoint here.

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
- Add the Ascend-style `redirectSoftSliceDirs(t)` (temp `OperatorLibDir`/`OperatorPodsDir`) + `slicedPod` /
  `newSlicedServer` fixtures to both new allocator test files (neither vendor has any allocator test today).
- Introduce the injectable seam per vendor — a fake sysfs root (MetaX) and a fake `smluDriver` (Cambricon) — plus a
  fake clock / deterministic tick source so the resync-ticker and debounce are testable without wall-clock waits.
- No detector test changes: detectors are hardware-gated and untested by precedent, and `CoresPercentageOvercommit`
  is already the zero-value `false` for both (no flip, unlike Hygon's C1).

#### Unit tests
Table-driven; local `darwin`, no hardware. Baselines measured 2026-07-18:
- `pkg/devicemanager/allocator/metax`: `2026-07-18` — `0.0%` → target ≥ 70%. `METAX_SGPUS` encoding; whole-card vs
  partial split; default cores%=100; missing-memory error; sliced vs exclusive/shared/visibility isolation; marker
  render/parse fail-closed (scoped per-card); registry∪markers slot derivation; idempotent reuse + changed-param
  fail-closed; reclaim (dead-pod destroy after 3 per-pod misses, subdevice-less-marker clean, marker-less-dead-UID
  destroy vs marker-less-live-UID left intact, sibling-marker never removed, resync-ticker-driven reclaim with zero
  broadcast ticks, startup re-seed); `-race` allocMu↔reclaim.
- `pkg/devicemanager/allocator/cambricon`: `2026-07-18` — `0.0%` → target ≥ 70%. `(cores%,memMiB)→SMluSet` mapping;
  bounded instance-name encode/decode (max UID + 63-char container, no collision); single-card reject; default
  cores%=100; missing-memory error; exact-match profile reuse + no shared-profile destroy; marker fail-closed;
  reclaim (same cases as MetaX over the fake driver); `-race`.
- `binding/cndev`: `2026-07-18` — `0.0%` → small (guard-path only). Each new sMLU wrapper returns
  `ERROR_FUNCTION_NOT_FOUND` when `so.Lookup` is nil (no real `.so`); the `SMluSet` field packing is asserted.
- `pkg/deviceplugin`: `2026-07-18` — `39.7%` → ≥ 39.7% (no regression). Server-level: MetaX/Cambricon `New()`
  register the Sliced server only when `!opts.NoSliced`; the bare `.sliced` resource appears in `ListAndWatch`.

Cross-check-driven scenario groups (Codex): **allocMu/reclaim races** (reclaim tick between create and marker rename
blocks on `allocMu`; empty-scan-before-Allocate; same-card concurrent no duplicate idx; sibling marker never
removed); **dropped-tick/debounce** (3 accepted-absent → reclaim once; absent,absent,live,absent,absent → no reclaim
before 3 fresh misses; dropped ticks neither increment nor reset; resync ticker still reclaims a starved dead slice);
**grace boundary** (terminating pod present every grace tick → no reclaim; disappears after grace → reclaim only
after 3 accepted-absent; force-delete-while-running → documented safety behavior); **crash/restart orderings** (every
ordering of {create, temp-write, rename, respond, crash} with kubelet NOT re-running Allocate → assert recover/leak/
wedge, and rule (c) never destroys a live-reserved slice); **idempotent changed-params** (same/changed cores/mem/card
→ one deliberate outcome, never mutate a live slice); **vendor caps/encoding** (MetaX fill 0–15, 17th rejected,
registry-object-without-marker not reselected, sched_class not mutated on a live card; Cambricon multi-card reject
before side effects, profile reuse only on exact match, shared-profile destroy safety, name-length/NUL bound).

#### Integration tests
`pkg/deviceplugin` server-level (fake reconciler + temp dirs): MetaX/Cambricon `New()` register Sliced only when
`!opts.NoSliced`; a Sliced `Allocate` routes to the new branch; the reclaim loop consumes the notifier **and** the
resync ticker and destroys a dead-pod subdevice over a fake driver/sysfs. Concrete test names added after the
implementation PR merges.

#### e2e tests
None — no vendor hardware exists, and the vendor runtime consuming the sysfs subdevice / cnDev instance / injected env
is a documented post-merge hardware phase. The shipped capability-feedback e2e already covers the `.sliced.*`
advertisement (no per-vendor isolation e2e is added here).

## Alternatives
- **List the driver/sysfs registry only (no on-disk marker), correlating via a pod-UID-encoded subdevice identity** —
  rejected as the primary mechanism: MetaX's sysfs sgpu subdevice may not support a free-form alias tag, making
  correlation fragile, and it loses the clean lowest-free slot ledger. The registry scan is kept as a *secondary*
  crash-orphan catch, and the embedded identity as a *secondary* correlation key where supported.
- **Extend the shared `podDirGC` with a vendor teardown callback** — rejected per the `2026-07-18` recorded decision:
  it couples the generic dir-GC to vendor driver/sysfs teardown; a distinct per-vendor reconciler keeps the concern
  where the vendor code lives.
- **Replicate the HAMi annotation protocol and let the vendor device-plugin create the subdevice** — rejected:
  contradicts the operator's NFD-counts + self-driven-injection philosophy and drags in the vendor scheduler /
  webhook / node lock. The operator drives the vendor primitive (sysfs / cnDev) directly.
- **Cambricon via the device-plugin `memUnit` annotation path instead of direct cnDev** — rejected: direct cnDev gives
  clean units (`memorySize` bytes, `mluQuota` percent), bypassing the 256/`memUnit` indirection and the
  `--min-dsmlu-unit` config dependency. The cost is a new (but contained, hand-written, dlopen-guarded) CGO wrapper
  surface — accepted.
- **Implement both vendors in parallel / Cambricon first** — rejected: MetaX (pure Go sysfs, fully fake-testable)
  lands the stateful template + reclaim reconciler with the least risk; Cambricon then layers the CGO wrapper surface
  on a proven lifecycle.

## Open Questions
- **MetaX sysfs contract (hardware).** Exact `sgpu/create` / `model` / `sched_class` sysfs paths + write format, the
  injected device-node set (control nodes + which `/dev/dri/*`), and whether the created subdevice exposes an alias
  tag the reclaim loop can read back — confirm on hardware; the seam isolates the impl so only `sgpu.go` changes.
- **Cambricon injection form (hardware).** Device-node injection (`/dev/cambricon_dev<slot>` / `_ipcm<slot>` /
  `cap_dev*_mi*`) vs the `--use-runtime` `VIRTUAL_DEVICES` env — which to default and how to detect `--use-runtime`;
  plus the cnDev `dynamic-smlu` version prerequisite probe.
- **cnDev exported wrapper surface (/my-plan).** Exactly which generated calls to wrap and their Go signatures /
  `SMluSet` field mapping — `cndevCreateSMluProfileInfo`, `cndevCreateSMluInstanceByProfileId`,
  `cndevDestroySMluInstanceByHandle` / `…ByInstanceName`, `cndevGetAllSMluInstanceInfo`, and whether `cndevSetSMLUMode`
  is needed to put a card into sMLU mode first.
- **Reclaim wiring (/my-plan).** Confirm the reclaim loop subscribes via `getReconcileNotifier(Manufacturer, Sliced)`
  (a second subscriber alongside the Sliced server's ListAndWatch) vs. a dedicated controller-runtime Pod reconciler
  in the device-manager; and the miss-debounce count (align with `podDirGCMaxMisses = 3`).
- **Hoisting a shared stateful-reclaim helper.** Whether to generalize the marker + reclaim scaffolding into
  `pkg/deviceplugin` now (two consumers) or keep it per-vendor until a third stateful vendor appears.
- **K1 — per-card `cores-percentage` accounting for `overcommit=false` vendors (deferred).** Two default-cores slices
  can co-locate and the second strands; the fix (per-card `cores` accounting / non-100 default) lives in the
  capability spec / Pod webhook.
- **K2 — webhook enforcement of `.sliced > 1` support per vendor (deferred).** The Pod webhook should reject
  `.sliced > 1` for non-NVIDIA vendors; documented in `docs/architecture.md`, a follow-up.
