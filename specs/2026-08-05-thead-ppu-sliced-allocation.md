# Spec: THead PPU logical slicing — the capability, from the image build to the allocator

Status: Building
Type: Feature

> **This spec ships the capability, not the library.** `specs/2026-08-03-thead-ppu-slicing-shim.md`
> delivered `csrc/thead/ppu-slicing-shim/` — both quota dimensions enforced per card, the cross-process
> ledger, the `dlsym` visibility hook, the reader — and handed off, deliberately, everything that turns
> those sources into something a user can request: the `pack/` stage that builds them, the
> `ld.so.preload` asset, the detector's `Status.LogicalSliced`, and the allocator's `Sliced` server.
> That handoff list is this spec's scope, file for file (that spec's Project Structure, "Handed off").
> Until it lands, a PPU slice happens only where somebody sets `LD_PRELOAD` by hand.

## Summary

GPUStack Operator discovers and allocates THead PPU cards in exclusive, shared and partitioned
(`.partitioned`, the vendor's own MIG) modes. It cannot slice one **logically**: the THead detector
advertises no `Status.LogicalSliced`, the THead allocator registers no `Sliced` server, and the
operator image builds none of the shim tree, so `${GPUSTACK_LIB_DIR}/thead/` does not exist. This spec
closes all three at once, because they only work together — a capability the image did not build is a
claim the node cannot honour, and a library nothing advertises is a library nothing mounts.

The shape is the NVIDIA branch's, which is the blueprint the shim was designed against: a builder stage
whose product lands under `${GPUSTACK_LIB_DIR}/<vendor>/`, the device-manager init container that stages
that tree onto the host, a per-card `Status.LogicalSliced` set in the same branch that rules out
partitioning, and an allocator branch that mounts the library plus a container-scoped
`/etc/ld.so.preload` and injects the vendor's own quota variables. Four things depart from it, each
forced by what the PPU deployment model or the shim itself actually is:

- **The shim is two shared objects, and their preload order is load-bearing.** `hggc_quota.so`
  enforces; `hgml_dlsym_hook.so` makes the quota visible to `ppu-smi`. Two libraries that interpose
  `dlsym` do not chain through each other, so the visibility half must come first or it is loaded,
  initialised and never entered — a silently physical figure rather than an error.
- **Devices are passed through, not injected by a runtime hook.** THead follows the ROCm model: the SDK
  lives in the workload container and the host only passes `/dev/alixpu`, `/dev/alixpu_ctl` and
  `/dev/alixpu_ppu<N>`. So the sliced response carries the same fail-closed device set the other modes
  build, and there is no `NVIDIA_VISIBLE_DEVICES` analogue and no `runtimeClassName`.
- **The compute figure is mandatory even when it is 100 %.** The variable shape is NVIDIA's — a per-card
  memory limit and one un-indexed compute limit — but the meaning of an absent compute figure is not:
  `SlicedCoresPercent` defaults to 100, so omitting the variable is indistinguishable from "no compute
  quota", and the shim refuses a card whose compute figure is missing rather than inheriting that
  default. HAMi-core does the opposite and falls back to a whole card's compute.
- **The ledger region is per container and must stay that way.** The NVIDIA branch mounts the host's
  `/dev/shm` so HAMi-core's cache is shared node-wide; the PPU region is addressed by
  **container-local** card index, so sharing it across containers would let two containers' index `0`
  charge the same slot. It gets a per-container directory under the pod work dir instead.

Acceptance stops at the image: unit tests, `make lint`/`make test`, and an assertion that the built
image's `${GPUSTACK_LIB_DIR}/thead/` carries the two shared objects, the reader and the preload file
with linkage intact. Running a sliced Pod on real hardware is a Non-Goal here and stated as one — the
shim's three PoC gates already PASSed on a live 16-card `PPU-ZW810E` host, so the mechanism is proven
and what this spec leaves unproven on hardware is the wiring, not the enforcement.

## Motivation

### Goals

- **Make `.sliced` requestable on a THead PPU node, end to end.** A user sets
  `Instance.Spec.Resources.AcceleratorSliced{Memory,Cores}Percentage`; the existing chain folds it into
  `.sliced.*` limits and Kueue credits; this spec supplies the two ends that are missing — a pool that
  advertises the capability and an `Allocate` that hands the container a real, enforced slice.
- **Build and stage the shim tree from the operator image.** `hggc_quota.so`, `hgml_dlsym_hook.so` and
  `ppu-monitor` compiled from `csrc/` — the repo's first library built from its own sources rather than
  a pinned upstream commit — installed into `${GPUSTACK_LIB_DIR}/thead/` beside a `thead/ld.so.preload`
  that names the container paths in the required order.
- **Advertise the capability only where the image actually built it.** The SDK is `linux/amd64` only, so
  the arm64 operator image ships no PPU shim; a detector that advertised slicing there would place a
  Pod that cannot start. The gate is the library's presence on disk, checked per detect pass.
- **Emit both quota dimensions in the NVIDIA branch's shape, and make both mandatory.** A per-card
  `HGGC_DEVICE_MEMORY_LIMIT_<i>` against that card's own VRAM plus one un-indexed `HGGC_DEVICE_SM_LIMIT`
  — the keys HAMi-core is given, aligned deliberately rather than improved on, since admission pins a
  logical slice to a single card and a per-card compute cap is therefore not requestable through any
  path. What is *not* aligned is the meaning of an absent figure: the shim refuses such a card, where
  HAMi-core hands out a whole one.
- **Keep logical and physical slicing mutually exclusive per card**, the way every other vendor does: a
  card in the partitioning mode reports partition profiles and no logical capability, and the reverse.
- **Leave every other mode untouched.** Exclusive, shared, visibility and partitioned responses,
  the credit accounting, the `.sliced.units` capacity fold, and the reclaim loop are unchanged; the
  regression evidence is that `make test` still passes with no edits to their tests.
- Success is testable: (1) `make lint` and `make test` pass, with new table-driven cases covering the
  detector gate and the allocator's response; (2) a locally built `linux/amd64` operator image carries
  `${GPUSTACK_LIB_DIR}/thead/{hggc_quota.so,hgml_dlsym_hook.so,ppu-monitor,ld.so.preload}`, each shared
  object's `DT_NEEDED` naming at most `libc.so.6` and no `GLIBC_` requirement above the SDK's floor;
  (3) an arm64 build of the same Dockerfile succeeds and leaves that directory without the shared
  objects, and the detector withholds the capability for exactly that reason; (4) `ppu-monitor` runs in
  a container with no SDK and no device.

### Non-Goals

- **Running a sliced Pod on PPU hardware.** Chosen deliberately: acceptance is unit tests plus image
  artifacts. The mechanism the slice depends on was measured on a live 16-card host by the shim spec's
  Gates 1–3 and cases 5–7 (per-card memory, per-card compute, two-process contention), all under a
  hand-set `LD_PRELOAD`. What this spec adds on top is Go that composes paths, figures and mounts —
  which unit tests can pin exactly — plus a Dockerfile stage whose product the image build itself
  asserts. A live run remains the obvious follow-up and is listed in Open Questions with what it would
  cover that nothing here does.
- **Opening multi-card logical slicing.** A sliced request is one card, pinned by two admission gates
  that say so deliberately (`worker/webhooks/worker/pod.go:492` — "multi-card logical slicing is
  deferred" — and `instance.go:873`). This spec aligns with that deferral instead of working around it:
  the allocator emits the same key shape the NVIDIA branch does, adds no card-count guard of its own, and
  changes nothing under `csrc/`, whose per-card support simply waits.
- **A metrics scraper over the ledger region.** The region's layout is already a documented contract and
  `ppu-monitor` already reads it; exposing it as a GPUStack metric is a separate effort with its own
  surface (there is no per-slice usage metric for any vendor today).
- **arm64 support for the shim.** The SDK ships `targets/x86_64-linux` and nothing else. The arm64
  operator image stays buildable and simply offers no PPU logical slicing.
- **Switching `gpustack-operator-xbuild-and-verify`'s `xbuild-thead-ppu` arm to buildx.** The shim spec
  anticipated it ("once that stage lands the arm can switch to buildx without moving anything else"),
  but the only host that runs the hardware cases has no docker and no running buildkitd, so switching
  would retire a verification path that works in exchange for one that cannot run there.
- **HGGC RT v2, physical slicing changes, and any tuning of the compute controller's gains.** The five
  `HGGC_SM_CONTROL_*` knobs are tuning rather than quota; the allocator injects none of them and the
  shim's defaults stand.
- **Turning logical slicing into a security boundary.** It stays cooperative isolation — static linking,
  musl, a direct `ioctl`, or removing the preload all bypass it — exactly as it is for NVIDIA and Ascend.
- **Changing how PPU cards are discovered or allocated in exclusive / shared / partitioned mode.**

## Proposal

A THead PPU card that is not in the partitioning mode advertises logical slicing, and a container that
requests a slice of it starts with the shim preloaded and its quota figures in its environment.

Three pieces, in the order they gate each other:

1. **The image builds the shim.** A `pack/` stage compiles the checked-in sources inside
   `gpustack/thead-ppu-devel:2.1.1` and installs the two preloaded objects plus the reader; the final
   image copies them to `${GPUSTACK_LIB_DIR}/thead/` and installs the `ld.so.preload` asset beside them.
   The device-manager's existing init container stages that tree onto the host unchanged.
2. **The detector advertises what the image built.** Per card, in the branch that already decides
   partitioning: a card in the mode keeps reporting only partition profiles; every other card reports
   `LogicalSliced{Count: 128, CoresPercentageOvercommit: true}` — but only where the staged library tree
   is actually present, so an arm64 node with a PPU card advertises nothing rather than advertising a
   slice it cannot start.
3. **The allocator hands out the slice.** A `Sliced` server behind `!opts.NoSliced`, whose response is
   the same fail-closed device set every other THead mode builds, plus the mounts that preload the shim
   and the environment that carries the quota — in the same key shape the NVIDIA branch uses, a per-card
   memory limit and one un-indexed compute limit, since admission pins a slice to a single card anyway.

Nothing upstream of `Allocate` changes: the request API, the Pod webhook's `.sliced.*` fold, the
`.sliced.units` capacity key, the Kueue credit accounting and the four-view status are all vendor-blind
and already carry THead.

### User Stories

#### Story 1

As a platform user running workloads on a THead PPU node, I want to request a fraction of a PPU card
with `.sliced.memory-percentage` and `.sliced.cores-percentage`, so that several workloads share one
card with predictable capacity instead of taking a whole card each.

#### Story 2

As a cluster administrator, I want a PPU card's logical-slicing capability to show up in the `Devices`
status and in the pool's InstanceType capability, so that the scheduling chain materializes sliced
flavors and queues without me configuring anything per node.

#### Story 3

As a GPUStack Operator maintainer, I want the operator image to build and stage the shim itself and the
detector to advertise slicing only where that build landed, so that the capability a node advertises and
the library a container mounts can never disagree — including on arm64, where the SDK does not exist.

#### Story 4

As a user debugging a slice, I want `ppu-smi` inside my container to report my own quota and a mounted
`ppu-monitor` to print both dimensions' quota and usage, so that I can tell a throttled workload from a
misconfigured one — the compute limit appears in no `ppu-smi` field at all.

### Core Features & Acceptance Criteria

#### F1 — `pack/` build, staging and the preload asset

- **`pack/gpustack-operator/external/thead/build-libvppu.sh`** — a thin wrapper, following
  `external/nvidia/build-libvgpu.sh`: it takes `<src-dir> <out-dir>`, delegates every compile decision
  to `csrc/thead/ppu-slicing-shim/build.sh` (`lib hggc_quota hgml_dlsym_hook`, then `tool ppu_monitor`,
  with `OUT` pointed at the output directory), and installs the three artifacts. It carries **no
  compile recipe of its own**: the tree's `build.sh` is the single place the translation-unit lists and
  flags live, and a second copy is exactly the drift the shim spec's T16 fixed. Unlike the NVIDIA and
  Ascend wrappers it clones nothing and takes no `ARG LIB_*_COMMIT` — the source is in-repo.
- **The wrapper asserts what it built, and fails the build otherwise**, mirroring
  `pack/thead-ppu-devel`'s own smoke-test convention and reusing the two properties
  `cases/thead-case-1.sh` already decides with `readelf`: each shared object's `DT_NEEDED` is empty or
  exactly `libc.so.6`, and its highest `GLIBC_` requirement does not exceed the SDK's floor
  (`GLIBC_2.17`). Both are the reason the shim can load in a workload container nobody controls, and a
  build that silently lost either would ship a library that fails at container start.
- **A `xbuild-thead-ppu` Dockerfile stage** on `gpustack/thead-ppu-devel:2.1.1`, installing to `/out`,
  arch-selected rather than platform-forced:
  - the amd64 stage builds the tree; the arm64 stage is a stand-in that produces an **empty** `/out`;
    a `FROM xbuild-thead-ppu-${TARGETARCH} AS xbuild-thead-ppu` alias picks between them, using
    BuildKit's pre-defined `TARGETARCH` with no declaration of its own. The unreferenced leg is pruned:
    an arm64 build never resolves the amd64-only devel image at all.
  - Why not `FROM --platform=linux/amd64`: buildx would resolve an amd64 manifest under emulation on the
    arm64 leg and the final arm64 image would then carry an amd64 `.so` that no container can load —
    a capability the detector's file check would wrongly accept. Why not simply omitting the stage: the
    `COPY --from` in the final stage is unconditional, so the alias is what keeps one Dockerfile
    building on both legs.
- **The final stage** copies `/out` to `${GPUSTACK_LIB_DIR}/thead` beside the Ascend and NVIDIA copies,
  and `install -D`s `rootfs/etc/gpustack/lib/thead/ld.so.preload` next to the existing pair.
- **`pack/gpustack-operator/rootfs/etc/gpustack/lib/thead/ld.so.preload`** names the two **container**
  paths:

  ```
  /usr/local/vppu/hgml_dlsym_hook.so
  /usr/local/vppu/hggc_quota.so
  ```

  **Their relative order is inert, and that was measured rather than assumed**: only
  `hgml_dlsym_hook.so` defines `dlsym` (`FUNC GLOBAL`), while `hggc_quota.so` merely imports it
  (`NOTYPE GLOBAL UND`), so the two do not contest the symbol and no build-time assertion over this
  file's line order would be testing anything. The ordering rule the shim spec records is about a
  **foreign** `dlsym` interposer, and this file cannot express it: mounting over `/etc/ld.so.preload`
  replaces whatever the workload image had there, so a peer can only arrive through `LD_PRELOAD` —
  processed *before* this file — or through the workload binary's own `DT_NEEDED`. Behind such a peer the
  visibility half is inert and `ppu-smi` reports the physical card. That stays a documented caveat, not
  something the build can enforce.
- Acceptance: `make package gpustack-operator` on an amd64 target produces an image where
  `${GPUSTACK_LIB_DIR}/thead/` holds `hggc_quota.so`, `hgml_dlsym_hook.so`, `ppu-monitor` and
  `ld.so.preload`; the same Dockerfile builds for `linux/arm64` with that directory holding only
  `ld.so.preload`; `ppu-monitor` executes in a container with no SDK and no device; the linkage
  assertions above are enforced inside the build rather than checked by hand afterwards.

#### F2 — Detector: per-card `Status.LogicalSliced`

- In `pkg/devicemanager/detector/thead/device.go`, the existing partitioning-mode branch gains its
  `else` arm — the NVIDIA detector's exact shape — so a card reports **either** partition profiles
  **or** logical slicing, never both and never neither by accident.
- The figure is `Count: 128, CoresPercentageOvercommit: true`. 128 mirrors the NVIDIA blueprint the shim
  was designed against; `hgml.h` documents no per-card user-process ceiling for PPU, and the count is a
  deliberately loose device-plugin token pool — the binding constraint is the `.sliced.units` memory
  budget — so the safe direction is generous: too small gates scheduling, too large does not.
  Overcommit is `true` because the compute cap is a duty-cycle window over wall time, so two slices may
  each ask for up to 100 %; memory is the non-oversubscribable dimension and is unaffected by the flag.
- **The capability is gated on the staged library tree existing.** Both shared objects must be present
  under `<OperatorLibDir>/thead/`, checked once per detect pass, not per card: either one alone is a
  broken slice (an inert visibility half reports the physical card; a missing enforcement half caps
  nothing). The staged path is checked rather than the in-image one because it is the exact file the
  allocator will mount, and the device-manager's init container completes before the detector runs, so
  the check is stable for the pod's lifetime.
- `device.SetGroupSlicedDetails(grpList)` is **already called** (added with the partitioning work), so
  the group-level aggregate follows automatically once the per-card field is set — the failure the shim
  spec warned about (`Logical.Count == 0` makes the pool silently un-sliceable) no longer needs a
  separate fix, and a test pins the aggregate rather than assuming it.
- Acceptance: table-driven detector cases assert (a) a not-partitioned card with the tree present
  reports `Logical.Count == 128` and the group aggregate sums it; (b) the same card with the tree absent
  reports zero and no aggregate; (c) a card in the partitioning mode reports profiles and zero logical
  capability with the tree present; (d) a card whose partitioning mode could not be read is treated as
  not partitioned, as it already is.

#### F3 — Allocator: the `Sliced` server and its injection

- `New` registers `newServer(logger, DeviceAllocationModeSliced, nil)` behind `!opts.NoSliced`, between
  the shared and partitioned servers, matching the NVIDIA ordering. The partition driver stays `nil` for
  it: a slice never touches the partitioning surface.
- `GetContainerAllocateResponse` keeps its single pass over the allocated cards but now collects the
  card/group pairs the sliced path needs, then branches on the mode — the NVIDIA and Ascend shape. The
  non-sliced response is byte-for-byte what it is today.
- **The sliced response is the plain device set plus injection**, because the vendor has no
  container-runtime hook: the two control nodes and each allocated card's node, resolved through the
  same fail-closed helpers the other paths use (a card whose recorded minor number cannot be proven is
  refused, not answered with a neighbour's node), and then:

  | | |
  |---|---|
  | `HGGC_DEVICE_MEMORY_LIMIT_<i>` | MiB, from `.sliced.memory-percentage` / `.sliced.memory-mib` against **that card's own group** VRAM; `<i>` is the loop position over the allocated cards |
  | `HGGC_DEVICE_SM_LIMIT` | percent, from `.sliced.cores-percentage`, un-indexed, **emitted even at 100** |
  | `HGGC_LEDGER_PATH` | the per-container region file inside the rw mount below |
  | `LIBHGGC_LOG_LEVEL` | `1` — denials and errors — injected only when the workload declares no value of its own |

- **The shape of that pair is the NVIDIA branch's, deliberately.** HAMi-core is given a per-card
  `CUDA_DEVICE_MEMORY_LIMIT_<i>` and a single un-indexed `CUDA_DEVICE_SM_LIMIT`
  (`allocator/nvidia/deviceplugin.go:276-287`), with `<i>` the loop position over the allocated cards —
  no ordinal sort and no card-count guard. This branch matches it key for key. The one thing it must
  **not** copy is the level HAMi-core's own default sets: absence of a compute figure means "no cap" to
  HAMi-core and "unusable card" to this shim, which is why the figure is emitted even at 100 %.
- **The un-indexed compute figure is a complete configuration, not a fallback.** The shim reads
  `HGGC_DEVICE_SM_LIMIT` as the cap for every card carrying no figure of its own, and nothing in the
  environment says how many cards a container holds — so one un-indexed figure caps all of them. The
  memory dimension emits no un-indexed form for the same reason NVIDIA's does not: each card's VRAM
  budget is computed against that card's own group.
- **`<i>` is a loop position, and today it can only ever be `0`.** Admission pins a logical slice to
  exactly one card — the Pod webhook requires `<base>.sliced` to be exactly 1
  (`worker/webhooks/worker/pod.go:492`, "multi-card logical slicing is deferred") and the Instance
  webhook requires `spec.resources.accelerator` to be exactly 1 for a sliced request
  (`instance.go:873` → `validateSingleCardRequest`). The container's single card is renumbered to index
  `0` inside it, which the shim spec measured on hardware. The loop is written to serve several cards
  anyway, exactly as NVIDIA's is, so that opening multi-card later is a change to the admission gates
  rather than to this branch.
- **Mounts**, all container-scoped:

  | Container path | Host path | Mode |
  |---|---|---|
  | `/etc/ld.so.preload` | `<OperatorLibDir>/thead/ld.so.preload` | ro |
  | `/usr/local/vppu/hgml_dlsym_hook.so` | `<OperatorLibDir>/thead/hgml_dlsym_hook.so` | ro |
  | `/usr/local/vppu/hggc_quota.so` | `<OperatorLibDir>/thead/hggc_quota.so` | ro |
  | `/usr/local/vppu/ppu-monitor` | `<OperatorLibDir>/thead/ppu-monitor` | ro |
  | `/var/run/vppu` | `<PodWorkDir>/run/vppu` | rw |

  `ppu-monitor` rides the library's own mount exactly as Ascend mounts `enpu-monitor`. The rw directory
  lives under the pod work dir so the existing per-pod GC reclaims it when the pod is gone, and it is
  **per container** on purpose: the region is addressed by container-local card index, so a node-wide
  location (the NVIDIA branch's host `/dev/shm`) would let two containers' index `0` charge one slot.
  No separate lock directory is needed — the shim's lock is an `fcntl` record lock inside the region
  file itself.
- **No card-count guard is added**, which is again the NVIDIA branch's choice: admission already pins a
  slice to one card, and re-checking it here would duplicate a gate that lives upstream — Ascend's
  refusal exists because `npu_info.config` physically models one NPU, a constraint this vendor does not
  have. The shim's own per-card keying stays as it is; nothing under `csrc/` changes.
- Acceptance: table-driven allocator cases assert the full response for one card, and a two-card case
  that pins the loop's shape the way NVIDIA's `TestGetSlicedContainerAllocateResponse_MultiCard` does
  (each memory figure against its own group's VRAM, one un-indexed compute figure) while noting in the
  case name that admission does not admit it today;
  compute emitted at `100` when nothing was requested; `LIBHGGC_LOG_LEVEL` injected when absent and
  left alone when the container declares it; an allocation refused when a card's minor number cannot be
  proven; the non-sliced modes' responses unchanged.

#### F4 — Documentation

- `README.md`'s accelerator matrix marks T-Head logical slicing supported, and the note under it stays
  accurate about what the compute budget means here (a duty-cycle cap, not a scheduling weight).
- `docs/architecture/discovery.md`: T-Head joins the preload-library row of the per-vendor mechanism
  table; the paragraph that names `libvgpu.so` / `libvruntime.so` gains the PPU pair and its ordering
  constraint; the quiet-logging paragraph gains `LIBHGGC_LOG_LEVEL=1` and says why it is `1` rather than
  `0` (our level 1 is per-denial, not per-call); "Where the preload libraries come from" records the one
  that is built from this repo's own `csrc/` tree rather than a pinned upstream commit, and that it is
  amd64-only.
- `docs/accelerator-requests.md`'s logical-slicing row names the THead library alongside the other two.
- Routed through the `gpustack-operator-docs` skill so the index, links and tables of contents are
  checked rather than assumed.

### Notes / Constraints / Caveats

- **Two shared objects, not one `libvppu.so`.** The shim spec's prose says "the library" throughout, but
  `csrc/thead/ppu-slicing-shim/build.sh` — the authority — produces `hggc_quota.so` and
  `hgml_dlsym_hook.so`, plus the `ppu-monitor` tool. This spec follows the build script.
- **The PPU deployment model is ROCm's, not NVIDIA's.** The SDK is in the workload container; the host
  passes device nodes. No host library injection, no runtime-major library subdirectory (so
  `${GPUSTACK_LIB_DIR}/thead/` is flat, unlike `nvidia/cuda-<major>` and `ascend/cann-<major>-<family>`),
  and no `runtimeClassName`.
- **glibc floor.** The shim loads inside a container the operator does not control, which is why it is
  built on the SDK's own old base and why the linkage assertions are part of the build rather than a
  review item. This is independent of the operator image's Ubuntu 24.04 runtime base.
- **Mounting `/etc/ld.so.preload` replaces whatever the workload image had there.** Inherited from the
  NVIDIA and Ascend branches and unchanged here; a workload that ships its own preload file loses it
  inside a sliced container, and a workload that ships its own `dlsym` interposer makes the visibility
  half inert. Both are worth noticing, neither is enforceable from inside the library.
- **Everything upstream of `Allocate` is vendor-blind and already carries THead**: the `.sliced.*`
  resource names derive from the manufacturer table, `SlicedCoresPercent` / `SlicedMemoryMib` are shared
  helpers, and pool capability is `SlicedDetail.Logical.Count > 0`.
- **That includes the one-card rule, which is why this branch never sees a second card.** Both admission
  gates enforce it — the Pod webhook's rule 2 requires `<base>.sliced` to be exactly 1
  (`pkg/worker/webhooks/worker/pod.go:492`) and the Instance webhook requires
  `spec.resources.accelerator` to be exactly 1 for a sliced request
  (`pkg/worker/webhooks/worker/instance.go:873`). It is a platform-level deferral, not a THead limitation,
  and the NVIDIA branch lives under it too: it also indexes memory by loop position and also emits a
  single un-indexed compute figure. Aligning means a future change to that rule lands in one place for
  both vendors instead of two shapes that have to be reconciled first.
- **The detector cannot import `pkg/deviceplugin`, so the staged path is named twice on purpose.**
  Measured rather than assumed: `pkg/deviceplugin` transitively links the stdlib `plugin` package, and
  `pkg/devicemanager/detector/thead` links `runtime/cgo` through `binding/hgml` — the combination that
  aborts a darwin test binary at load. Sharing the constant the other way (moving `OperatorLibDir` into
  `pkg/device`) would rewrite ~110 references across 21 files, every vendor's allocator tests included, for
  one string. So the detector carries its own redirectable var and a comment naming the reason.
- **The helper asymmetry is the reason the compute figure is mandatory.** `SlicedMemoryMib` errors when
  neither memory request is present; `SlicedCoresPercent` returns 100. The shim refuses to reproduce
  that default, so the allocator must always emit the compute figure.
- **CI already builds this Dockerfile on both legs** on every push and pull request, so the new stage is
  covered — at the cost of pulling the SDK devel image on the amd64 leg. The stage is a leaf that only
  one `COPY` depends on, and the registry cache the image workflow already writes covers repeat builds.
- Local iteration: masking the `COPY --from=xbuild-*` lines for the vendors a change does not touch cuts
  a package build from ~20 GB of pulls to minutes; the THead stage is the one that must stay unmasked
  here, and the Dockerfile must be restored before committing.

### Boundaries

- **Always:** keep every preload container-scoped; keep each shared object's `NEEDED` at `libc.so.6` or
  nothing and assert it inside the build; emit both quota dimensions per card, compute included at
  100 %; advertise logical slicing only where the staged library tree exists; keep a card's logical and
  physical capabilities mutually exclusive; keep the ledger region per container.
- **Ask first:** publishing any image; anything that runs on, deploys to, or consumes a card on the PPU
  host; changing the `ld.so.preload` order; adding a compile recipe anywhere other than the tree's own
  `build.sh`.
- **Never:** write the host's `/etc/ld.so.preload`; replace or shadow a host library; inject the slicing
  preload into the device-manager process itself; let a missing or unparsable quota degrade into "no
  limit" — in particular, never omit the compute figure and inherit `SlicedCoresPercent`'s default of a
  whole card; duplicate an admission gate inside the allocator; change anything under `csrc/`; ship an
  amd64 shared object inside the arm64 image.

### Risks and Mitigations

- The container-local index the SDK assigns does not match the loop position the allocator emitted a
  memory figure against, so a card is held to another card's quota → **not reachable today**: admission
  pins a logical slice to exactly one card (`worker/webhooks/worker/pod.go:492`, `instance.go:873`), that
  card is renumbered to `0`, and the compute figure is un-indexed and therefore index-independent. The
  risk becomes live only if multi-card logical slicing is opened, and it lands on the NVIDIA branch at
  the same moment and in the same way — which is the second reason to hold the same shape rather than
  invent a THead-only ordering rule that would have to be reconciled with NVIDIA's later.
- The arm64 image ships an amd64 `.so` and the detector accepts it because the file exists → the arm64
  leg builds an **empty** `/out` through a `TARGETARCH` stage alias rather than pulling the amd64 image
  under emulation, so there is no file to accept.
- A capability change never reaches the `Devices` status, because the re-detect trigger ignores slicing
  capability and the align path can discard an updated group → not reachable through this gate: the init
  container stages the tree before the detector's first pass and nothing changes it while the pod lives,
  so the check answers the same way for the pod's lifetime. The underlying staleness is a pre-existing
  defect and stays out of scope.
- The two preloads are ordered correctly by us and then a workload image's own `dlsym` interposer wins
  the race anyway → nothing inside the library can detect it; the constraint is documented in
  `discovery.md` and the asset's ordering is what we control. `ppu-smi` then reports the physical card
  while enforcement still holds, so the failure mode is a wrong *display*, not a leaked quota.
- A sliced container starts, the mounts resolve, and the shim then refuses every allocation because a
  figure is missing → the allocator emits a complete indexed pair per card and the unit tests assert the
  exact environment map, which is the only place this can be caught before hardware.
- The SDK devel image pull makes every CI image build slower → amd64 leg only, leaf stage, registry
  cache. If it becomes a problem the mitigation is mirroring the devel image, not dropping the stage.
- `${GPUSTACK_LIB_DIR}/thead/` is staged but a workload's `/usr/local` shadows the mount point → the
  mounts are individual files under a directory the vendor SDK does not own (`/usr/local/vppu`), and a
  collision would fail the container start loudly rather than silently disable the shim.
- An asynchronous free is refunded when it is enqueued rather than when it completes, so a container can
  transiently hold more than its quota → inherited from the shim, recorded there as a follow-up, and
  bounded by what the driver will actually hand out. Not re-litigated here.
- A process that launches kernels but never allocates is left out of the compute feedback → same:
  inherited, recorded, out of scope.
- Nobody notices that the whole capability was never exercised on hardware → it is a stated Non-Goal
  with an Open Question naming what a live run would cover, rather than an omission the spec is silent
  about.

## Design Details

### Commands

**Environment, pinned.** The Go half runs **locally**: the whole module — including the CGO vendor
detectors — builds and tests on the development darwin machine, so no linux host or SDK is needed for
anything under `pkg/`. The baseline this plan starts from, measured:
`pkg/devicemanager/allocator/thead` 88.9 %, `pkg/devicemanager/detector/thead` 35.2 % of statements.

The image half is `linux/amd64`-only and splits in two: the **stage-level** build runs locally under
emulation (it compiles twelve C files, which emulation handles fine) and is the fast loop; the **full
image** is built on a **remote amd64 builder host reached over SSH**, because a local full build pulls
~20 GB of vendor base images and can fill the Docker VM's sparse backing volume. A remote `make` needs a
login shell (`ssh <host> bash -lc '…'`) or the Go toolchain is off `PATH`.

```bash
# ---- Go verification, local (whole module builds and tests on darwin) ----
make lint          # whole-module golangci-lint + goimports; EDITS files; slow on a cold cache
make test          # go test -failfast -race -cover -timeout=30m; any args are EXCLUSION regexes
go test -count=1 -cover ./pkg/devicemanager/detector/thead/... ./pkg/devicemanager/allocator/thead/...

# ---- stage-level image build, local and emulated: the fast loop for T1 ----
# Check the Docker VM's backing volume first: the VM's own `df` misreports a sparse
# Docker.raw, and filling that volume corrupts the VM.
df -h /System/Volumes/Data

# The amd64 arm compiles the tree; the arm64 arm must produce an empty /out.
docker buildx build -f pack/gpustack-operator/Dockerfile --target xbuild-thead-ppu \
    --platform linux/amd64 --load -t thead-xbuild:amd64 .
docker run --rm --platform linux/amd64 thead-xbuild:amd64 bash -c \
    'ls -la /out; readelf -d /out/*.so | grep -E "NEEDED|Dynamic section"'
docker buildx build -f pack/gpustack-operator/Dockerfile --target xbuild-thead-ppu \
    --platform linux/arm64 --load -t thead-xbuild:arm64 .
docker run --rm --platform linux/arm64 thead-xbuild:arm64 bash -c 'ls -A /out | wc -l'   # expect 0

# ---- full image, on the remote amd64 builder: the F1 acceptance ----
ssh <builder> bash -lc 'cd <checkout> && make package gpustack-operator'
docker run --rm --platform linux/amd64 <image> bash -c '
    ls -la /etc/gpustack/lib/thead
    head -2 /etc/gpustack/lib/thead/ld.so.preload
    /etc/gpustack/lib/thead/ppu-monitor; echo "rc=$?"'
# ppu-monitor has no --help and no region to read here: what the run proves is that it
# REACHES its own "no usage region" message instead of dying in the loader, which is the
# whole claim -- it needs neither the SDK nor a device.

# ---- the shim tree itself, unchanged by this spec but the thing being packaged ----
cd csrc/thead/ppu-slicing-shim && ./build.sh unit && ./vppu_test    # no SDK, no device
docker run --rm --platform linux/amd64 -v "$PWD:/work" -w /work \
    gpustack/thead-ppu-devel:2.1.1 bash -lc './build.sh lib && ./build.sh tool'

# ---- chart, unchanged; run to prove it ----
make lint chart    # offline chart checks (NOT `make test chart`, which mutates a live cluster)
```

`make generate` is **not** needed: no API type changes: `AcceleratorLogicalSliced` and every `.sliced.*`
resource name already exist and already derive for THead.

### Project Structure

```
pack/gpustack-operator/Dockerfile                       # + xbuild-thead-ppu-{amd64,arm64} stages and the
                                                        #   TARGETARCH alias, + COPY to
                                                        #   ${GPUSTACK_LIB_DIR}/thead, + install -D of the
                                                        #   preload asset beside the ascend/nvidia pair
pack/gpustack-operator/external/thead/build-libvppu.sh  # NEW: wrapper over csrc/.../build.sh + the
                                                        #   DT_NEEDED and GLIBC floor assertions
pack/gpustack-operator/rootfs/etc/gpustack/lib/thead/ld.so.preload
                                                        # NEW: the two container paths of the preloads
pkg/devicemanager/detector/thead/device.go              # + Status.LogicalSliced in the else arm of the
                                                        #   partitioning branch, gated on the staged tree
pkg/devicemanager/detector/thead/device_test.go         # NEW (the package has only mig_profile_test.go
                                                        #   today): the gate's arms and their exclusivity
pkg/devicemanager/allocator/thead/deviceplugin.go       # + Sliced server behind !opts.NoSliced,
                                                        #   + the sliced branch: per-card envs + mounts
pkg/devicemanager/allocator/thead/deviceplugin_test.go  # + the sliced response cases
README.md                                               # matrix row
docs/architecture/discovery.md                          # mechanism table, env names, ordering, sources
docs/accelerator-requests.md                            # library name in the logical-slicing row
specs/2026-08-05-thead-ppu-sliced-allocation.md
```

### Code Style

The allocator's sliced branch follows the NVIDIA one key for key, so that the one difference reads as
deliberate: a per-card memory limit, a single un-indexed compute limit, and that compute limit emitted
even when it is a whole card's worth.

```go
// The variable shape is HAMi-core's, deliberately: a per-card memory limit indexed by loop
// position, and ONE un-indexed compute limit which the shim reads as the cap for every card
// carrying no figure of its own. Admission pins a logical slice to a single card, so the
// index is 0 today; the loop is written for several anyway, exactly as the NVIDIA branch's is.
//
// What is NOT copied from HAMi-core is what an absent compute figure means. It defaults to a
// whole card's compute there and makes the card unusable here, and SlicedCoresPercent returns
// 100 when nothing was requested — so the figure is emitted even at 100, because omitting it
// would be indistinguishable from "no compute quota" to everything downstream.
coresPct := deviceplugin.SlicedCoresPercent(ctr, coresRes)
envs["HGGC_DEVICE_SM_LIMIT"] = strconv.Itoa(coresPct)
for i := range accels {
    memMib, err := deviceplugin.SlicedMemoryMib(ctr, memPctRes, memMibRes, int64(accels[i].group.Memory))
    if err != nil {
        return nil, fmt.Errorf("derive sliced memory limit: %w", err)
    }
    envs["HGGC_DEVICE_MEMORY_LIMIT_"+strconv.Itoa(i)] = strconv.FormatInt(memMib, 10)
}
```

The MiB figure carries no unit suffix, unlike the NVIDIA branch's `"…m"`: HAMi-core parses a suffix,
the shim parses a bare MiB integer (`csrc/thead/ppu-slicing-shim/README.md`).

Conventions this follows: snake_case multi-word Go filenames; comments that state *why* a line is the
way it is rather than what it does; errors wrapped with the operation that failed; no helper extracted
for a single call site. Dockerfile conventions follow `pack/gpustack-operator/Dockerfile` — `ARG`s at the
top, a heredoc `RUN` whose body is verbatim, `set -exo pipefail`.

### Implementation Plan

Four tasks. **T1, T2 and T3 have no dependency on each other** — they touch three disjoint trees, and the
allocator never reads the detector's gate — so they can run concurrently; T4 documents what the three of
them landed. T1 is the one carrying an unretired *build-system* unknown (whether a `TARGETARCH`-selected
`FROM` alias resolves, and whether the unreferenced leg is pruned rather than resolved), so it goes first
in practice even though nothing blocks on it.

- [x] **T1 · The image builds and stages the shim**
      Blocked by: None
      Owns: `pack/gpustack-operator/Dockerfile`, `pack/gpustack-operator/external/thead/**`,
      `pack/gpustack-operator/rootfs/etc/gpustack/lib/thead/**`
      Gate: review
      Acceptance: `external/thead/build-libvppu.sh <src-dir> <out-dir>` delegates every compile decision to
      `csrc/thead/ppu-slicing-shim/build.sh` (`lib hggc_quota hgml_dlsym_hook`, then `tool ppu_monitor`,
      with `OUT` pointed at the output directory) and carries no recipe of its own; it installs the three
      artifacts and **fails the build** when a shared object's `DT_NEEDED` names anything other than
      `libc.so.6` or its highest `GLIBC_` requirement exceeds `GLIBC_2.17` — the two `readelf` assertions
      `pack/thead-ppu-devel/Dockerfile:199` and `:210` already make for that image's own smoke object.
      `xbuild-thead-ppu-amd64` builds the tree inside `gpustack/thead-ppu-devel:2.1.1`;
      `xbuild-thead-ppu-arm64` is a `${UBUNTU_IMAGE}` stand-in producing an **empty** `/out`; and
      `FROM xbuild-thead-ppu-${TARGETARCH} AS xbuild-thead-ppu` selects between them, and needs **no**
      `ARG TARGETARCH` declaration to do it: BuildKit's pre-defined platform arguments are already in
      global scope, which is why the three existing declarations (`:28`, `:93`, `:323`) exist at all —
      re-declaring one inside a stage is what makes its value readable *there*. The final stage copies `/out` to
      `${GPUSTACK_LIB_DIR}/thead` beside the ascend/nvidia copies and `install -D`s the preload asset
      beside their pair at `:469-474`. No assertion over that file's line order: only the visibility half
      defines `dlsym`, so the two entries do not contest it and their order decides nothing.
      Verify: the two stage-level `docker buildx build --target xbuild-thead-ppu` runs under Commands (amd64
      lists three artifacts with clean linkage, arm64 lists none), then the remote full-image build and its
      `ls` / `head -2` / `ppu-monitor` run

- [ ] **T2 · The detector advertises the capability, gated on the staged library**
      Blocked by: None
      Owns: `pkg/devicemanager/detector/thead/device.go`, `pkg/devicemanager/detector/thead/device_test.go`
      Gate: review
      Acceptance: the per-card sliced decision is collected into one function taking the partitioning mode,
      the detected profile list and the library directory, and returning the physical/logical pair — a small
      prefactor that buys the thing the inline branch cannot have: a test that pins **mutual exclusivity**
      rather than trusting an `else`. Logical is `{Count: 128, CoresPercentageOvercommit: true}` and is
      returned only when **both** `hggc_quota.so` and `hgml_dlsym_hook.so` are present under the staged
      directory, checked once per detect pass. That directory is a redirectable package var in this package,
      **not** `deviceplugin.OperatorLibDir`, and the comment says why: `pkg/deviceplugin` transitively links
      the stdlib `plugin` package while this package links `runtime/cgo` through `binding/hgml`, and that
      combination aborts the darwin test binary at load. Cases: both files → 128; either one alone → zero;
      neither → zero; partitioning mode on → profiles and zero logical, with the tree present, so the arms
      are shown to exclude each other rather than to differ.
      Verify: `go test -count=1 -cover ./pkg/devicemanager/detector/thead/...` (coverage above the 35.2 %
      baseline; the new function is fully covered, `DetectAccelerator` itself stays untestable without a card)

- [ ] **T3 · The allocator hands out the slice**
      Blocked by: None
      Owns: `pkg/devicemanager/allocator/thead/deviceplugin.go`,
      `pkg/devicemanager/allocator/thead/deviceplugin_test.go`
      Gate: review
      Acceptance: `New` registers `newServer(logger, DeviceAllocationModeSliced, nil)` behind
      `!opts.NoSliced`, between the shared and partitioned servers. `GetContainerAllocateResponse` resolves
      the two control nodes **first** (preserving today's error precedence, which
      `TestGetContainerAllocateResponse` pins), then collects the `(group, accelerator)` pairs through the
      existing `requireCardNode` guard, then branches on the mode. The sliced branch returns that device
      set plus the five mounts and an env map whose shape is the NVIDIA branch's key for key
      (`allocator/nvidia/deviceplugin.go:276-287`): `HGGC_DEVICE_MEMORY_LIMIT_<i>` per card, `<i>` the loop
      position, derived against **that card's own group** VRAM and carrying **no** unit suffix; one
      un-indexed `HGGC_DEVICE_SM_LIMIT` emitted even at 100; `HGGC_LEDGER_PATH`; and `LIBHGGC_LOG_LEVEL=1`
      only when the container declares no value of its own. No ordinal sort and no card-count guard — both
      would be THead-only inventions, and admission already pins a slice to one card. Cases: one card; a
      two-card case pinning the loop's shape as NVIDIA's `..._MultiCard` test does, named so it is clear
      admission does not admit it today; compute at 100 when nothing was requested; a memory error when
      neither memory request is present; the declared log level left alone; a card whose minor number
      cannot be proven refused; and the non-sliced modes' responses unchanged.
      Verify: `go test -count=1 -cover ./pkg/devicemanager/allocator/thead/...` (at or above the 88.9 %
      baseline)

**Checkpoint.** `make lint && make test` clean, and the remote amd64 image carrying all four files in
`${GPUSTACK_LIB_DIR}/thead/`. Only then is the capability real end to end, and only then does documenting
it describe something that exists.

- [ ] **T4 · Documentation**
      Blocked by: T1, T2, T3
      Owns: `README.md`, `docs/architecture/discovery.md`, `docs/accelerator-requests.md`
      Gate: (none)
      Acceptance: the README matrix marks T-Head logical slicing supported and the note under it stays
      accurate about what the compute budget is here (a duty-cycle cap, not a scheduling weight);
      `discovery.md` adds T-Head to the preload-library row of the per-vendor mechanism table, names the
      PPU pair where it names `libvgpu.so` / `libvruntime.so` — including the caveat that a workload
      image shipping its own `dlsym` interposer through `LD_PRELOAD` makes the visibility half inert —
      records
      `LIBHGGC_LOG_LEVEL=1` and why it is `1` rather than `0` (per-denial, not per-call), and records in
      "Where the preload libraries come from" the one library built from this repo's own `csrc/` tree
      rather than a pinned upstream commit, amd64-only; `accelerator-requests.md`'s logical-slicing row
      names the THead library alongside the other two. Routed through the `gpustack-operator-docs` skill.
      Verify: the `gpustack-operator-docs` skill's index / link / table-of-contents checks pass, and
      `make lint` still passes — the repo has no doc linter, so those checks plus a read of the rendered
      tables are the whole gate

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- **`TestNew_ServerSet`'s case `"--no-sliced changes nothing: this vendor has no logical slicing"`**
  (`pkg/devicemanager/allocator/thead/deviceplugin_test.go`) asserts the opposite of what this spec builds.
  It is rewritten in T3 to assert the sliced server is registered by default and dropped under
  `--no-sliced`, which is also the one place a regression that silently stops registering it would show.
- **A sliced-path fixture helper** in the same file, modelled on the NVIDIA test's
  `redirectLogicalSliceDirs` (`allocator/nvidia/deviceplugin_test.go:29`): redirect
  `deviceplugin.OperatorLibDir` / `OperatorPodsDir` into `t.TempDir()` and write the staged library files
  there, composed with the existing `redirectNodeRoots` so a sliced case still gets its faked character
  device nodes and minors.
- **`pkg/devicemanager/detector/thead/device_test.go` does not exist** — the package's only test file today
  is `mig_profile_test.go`. T2 adds it, following that file's table-driven shape.

#### Unit tests

- `pkg/devicemanager/allocator/thead`: `2026-08-05` - `88.9%` (baseline; must not regress, and the sliced
  branch and its ordering rule are covered by the T3 cases)
- `pkg/devicemanager/detector/thead`: `2026-08-05` - `35.2%` (baseline; the T2 function is covered in full.
  The package ceiling stays low because `DetectAccelerator` and `MonitorAccelerator` need a real card, which
  is why the sliced decision is extracted rather than tested through them)
- `pkg/deviceplugin`, `pkg/device`, `pkg/worker/controllers/worker`: unchanged, and that is the claim —
  every capability, credit and capacity path this feature rides is vendor-blind and already covered. A
  diff touching them means the design drifted.

#### Integration tests

The integration surface here is the **image**, not a cluster, and it is asserted at two levels:

- **Stage level, both platforms** (local, emulated): `--target xbuild-thead-ppu --platform linux/amd64`
  yields `hggc_quota.so`, `hgml_dlsym_hook.so` and `ppu-monitor` with `DT_NEEDED` at `libc.so.6` or empty
  and no `GLIBC_` requirement above `GLIBC_2.17` — assertions that live *inside* the build, so a regression
  fails the build rather than a later read; `--platform linux/arm64` yields an empty `/out`.
- **Image level** (remote amd64 builder): `${GPUSTACK_LIB_DIR}/thead/` carries those three plus
  `ld.so.preload`; the preload file's first line is the visibility shim; `ppu-monitor` runs to its own
  "no usage region" message rather than dying in the loader.
- `make lint chart` — the chart is untouched; this is the evidence, not a change.
- **No live-cluster integration test.** Nothing in this feature is reachable without a PPU card: the
  device-plugin registration, the pool capability and the injection all need a node whose detector saw a
  card. A fake-client integration test would only re-assert the unit tests through more machinery.

#### e2e tests

**None, deliberately, and the gap is named rather than papered over.** `gpustack-operator-e2e` needs a
cluster with the accelerator it is exercising, and this capability needs a PPU node — the one available host
runs production inference on 16 cards. What *is* already covered on that hardware is the half that carries
the risk: the shim's seven `gpustack-operator-xbuild-and-verify` THead cases PASSed there, including per-card
memory quotas across four allocation paths (case 3), two cards at different memory *and* different compute
figures in one container (cases 5 and 6 Part C), compute throttling under real kernel load (case 7), and the
`dlsym` preload ordering in both directions (case 2) — all under a hand-set `LD_PRELOAD`, which is exactly
the injection this spec automates.

What no test here reaches, and what an e2e run would be for: the mount set as the kubelet actually applies
it, and `ppu-smi` reading the ledger's figure through the full injection rather than a hand-set
`LD_PRELOAD`. That run needs a published dev operator image and a deployment on the PPU host; it is listed
in Open Questions as its own follow-up. Note what is *not* on that list any more: which container-local
index a two-card container assigns, because admission admits no such container — and the one card it does
admit is renumbered to `0`, which the shim spec already measured.

## Alternatives

- **One merged shared object instead of two.** Technically possible, but the visibility half must be
  able to win the `dlsym` race independently of the enforcement half, and the tree already ships two
  objects with their own translation-unit lists. Merging them would mean editing the shim tree — which
  this spec deliberately does not touch — for no gain.
- **Gate the detector on `runtime.GOARCH` rather than on the library's presence.** Cheaper, and wrong in
  the interesting cases: a partial build, a staging failure, or a hand-modified image all produce an
  amd64 node with no library, which GOARCH cannot see and a file check can.
- **Mount the host's `/dev/shm` and keep the shim's default ledger path**, the NVIDIA branch's shape.
  Rejected because the region is addressed by container-local card index: two containers' index `0`
  would charge the same slot, so a node-wide region is not a sharing optimisation but a correctness bug.
- **Emit `HGGC_DEVICE_SM_LIMIT_<i>` per card instead of one un-indexed figure**, which is what the shim
  spec's F5 hoped this handoff would do — it built the indexed compute key precisely because NVIDIA's
  branch emits only the un-indexed one. Rejected on alignment: admission pins a logical slice to one card
  (`worker/webhooks/worker/pod.go:492`, `instance.go:873`), so a per-card compute cap is not requestable
  through any path, and a THead-only key shape would then differ from NVIDIA's for a capability neither
  can express. The shim keeps its indexed support — nothing under `csrc/` changes — and it becomes
  reachable the day multi-card logical slicing is opened, for both vendors at once.
- **Refuse a multi-card sliced allocation in the allocator**, the Ascend branch's shape. Rejected as a
  duplicated gate: Ascend refuses because `npu_info.config` physically models one NPU, a constraint this
  vendor does not have, and the count is already pinned upstream at admission where the NVIDIA branch
  also leaves it.
- **Advertise a smaller per-card slice count (16 or 32).** 32 is the region's per-card process-slot
  bound, which is a per-*container* limit and does not bound containers per card; 16 matches most
  vendors but would gate scheduling on a 98 GB card long before memory does. The count is loose by
  design, so the safe direction is generous.
- **Land the Go work now and the `pack/` wiring later.** Rejected for the reason the shim spec gave when
  it handed both off together: a detector that advertises a library the image never built places Pods
  that cannot start, so the capability and its build are one change.
- **Verify on the PPU host as part of this spec.** Deliberately deferred (see Non-Goals and Open
  Questions): the mechanism is already proven on that host, and what remains is wiring that unit tests
  pin exactly.

## Open Questions

- **What a live run would add, and when to take it.** A sliced Pod on the PPU host would exercise two
  things nothing here does: the mount set as the kubelet actually applies it, and `ppu-smi` reading the
  ledger's figure through the full injection rather than a hand-set `LD_PRELOAD`. It needs a published dev
  operator image and a deployment on that host. Worth scheduling as its own follow-up.
- **When multi-card logical slicing is opened, who reconciles the index?** The deferral is recorded
  upstream (`worker/webhooks/worker/pod.go:492`), and lifting it makes the loop position load-bearing for
  the first time — for this vendor *and* for NVIDIA, which indexes by the same loop position. Whoever
  lifts it owns that question for both, plus whether the compute cap becomes per card (the shim already
  supports it; HAMi-core does too, through `CUDA_DEVICE_SM_LIMIT_<i>`).
- **Is 128 the right per-card ceiling?** It is loose by design and nothing measures the real number
  today. If a PPU per-card context or process limit turns up (or a live run finds one), the figure
  should follow it.
- **Should the ledger region move somewhere a host-side scraper can read it?** It is under the pod work
  dir today, which a host process *can* read; whether that is the interface a future metrics path wants
  is a decision for whoever builds it.
- **Does `ppu-monitor` belong on `PATH` inside the container** rather than at `/usr/local/vppu/`? Ascend
  mounts `enpu-monitor` beside its library and leaves it off `PATH`; matching that is the default here,
  but a user typing `ppu-monitor` is the more obvious surface.
