# Spec: AMD GPU logical slicing — the capability, from the image build to the allocator

Status: Building
Type: Feature

> **This spec ships the capability, not the library.** `specs/2026-08-06-amd-gpu-slicing-shim.md`
> delivered `csrc/amd/rocm-slicing-shim/` — the per-card VRAM quota, the cross-process ledger, the
> reported-capacity surface, the mask self-check probe — and handed off, deliberately and with stated
> reasons, everything that turns those sources into something a user can request: the `pack/` stage that
> builds them, the `ld.so.preload` asset, the detector's `Status.LogicalSliced`, the allocator's `Sliced`
> server, and `cumask.go` — the CU-mask derivation, which is consumed by the allocator and never by the
> library. That handoff list is this spec's scope, file for file (that spec's Project Structure, "Handed
> off"). Until it lands, an AMD slice happens only where somebody sets `/etc/ld.so.preload` and
> `HSA_CU_MASK` by hand.

## Summary

GPUStack Operator discovers and allocates AMD GPUs in exclusive, shared and visibility modes. It cannot
slice one **logically**: `pkg/devicemanager/detector/amd/device.go` never fills `Status.LogicalSliced`
and never calls `device.SetGroupSlicedDetails`, `pkg/devicemanager/allocator/amd/deviceplugin.go`
registers no `Sliced` server and discards the Pod and container it is handed (`:98-99`), and the
operator image builds none of the shim tree, so `${GPUSTACK_LIB_DIR}/amd/` does not exist. AMD is the
last of the nine supported manufacturers without the capability. This spec closes all of it at once,
because the pieces only work together — a capability the image did not build is a claim the node cannot
honour, and a library nothing advertises is a library nothing mounts.

The shape is the THead branch's, which is itself the NVIDIA branch's: a builder stage whose product lands
under `${GPUSTACK_LIB_DIR}/<vendor>/`, the device-manager init container that stages that tree onto the
host, a per-card `Status.LogicalSliced`, and an allocator branch that mounts the library plus a
container-scoped `/etc/ld.so.preload` and injects the quota. **One thing departs from it, and it is the
whole difficulty of this spec:**

- **The compute quota is not a variable handed to the library — it is a bitmask this repository has to
  compute.** THead and NVIDIA emit a percentage and let their shim throttle. AMD's platform enforces
  compute in hardware through `HSA_CU_MASK`, read by ROCr before any preloaded code exists, so the
  operator must derive the mask itself: two arithmetics that share nothing — RDNA allocates in WGP pairs
  aligned to the shader-engine count; the CDNA/GCN family has no pairing but interleaves mask bits across
  XCCs, which makes the atom `NUM_XCC` CUs and makes covering *every* XCC mandatory — against a platform
  that **fails open silently**. A mask ROCr rejects produces no error, no log line and no changed return
  code, and the container simply gets the whole card.
- **The two arithmetics are selected by the GPU's architecture family, not by its XCC count.** This is
  the correction a design review caught, and it is load-bearing: `gfx90a` (MI210, and each GCD of an
  MI250X) is CDNA silicon that reports **`NUM_XCC = 1`**, so an `NUM_XCC > 1` discriminator routes it
  into the RDNA branch and applies WGP pairing to a part that has no WGPs. The shim tree targets that
  part explicitly (`csrc/amd/rocm-slicing-shim/build.sh:55`), and no conformance row covers it. The
  branch is therefore keyed on the gfx family, and `NUM_XCC` is only the CDNA branch's atom size.
- **A derived mask needs a placement, and a placement needs a ledger.** A mask is not just a size, it is
  a position on the card, and two containers handed the same position share it instead of the card:
  measured, two 50 % tenants on the same half settle at 25.8 % each while the other half of the card sits
  idle. So this spec adds a per-card CU-occupancy ledger — carried on the same annotation transport and
  reservation union the MIG placement ledger already runs on, with no new API field — and packs slices
  disjointly while capacity allows, overlapping only when the card is oversubscribed.

Everything else is the THead template applied file for file: one builder stage (no per-runtime fan-out —
the library links no ROCm object, so one build serves every ROCm version), an `x86_64`-only product
selected by `TARGETARCH`, a per-container ledger directory under the pod work dir, and a detector arm
that carries no gate.

Acceptance goes one step past THead's: unit tests, `make lint` / `make test`, the built image's
`${GPUSTACK_LIB_DIR}/amd/` — **and a live run on the RDNA host this project owns**, because three things
in the injection contract were never measured (the `ROCR_VISIBLE_DEVICES` value format, the alignment
between that list and `HSA_CU_MASK`'s `GPU_list` index, and whether a *packed* mask at a non-zero offset
confines the way an offset-0 mask does). The first of those is a **prerequisite**, not an acceptance
item: the allocator cannot compose its three-variable tuple until the device-identity format is known.

## Motivation

### Goals

- **Make `.sliced` requestable on an AMD GPU node, end to end.** A user sets
  `Instance.Spec.Resources.AcceleratorSliced{Memory,Cores}Percentage`; the existing vendor-blind chain
  folds it into `.sliced.*` limits and Kueue credits; this spec supplies the two ends that are missing —
  a pool that advertises the capability and an `Allocate` that hands the container a real, enforced
  slice.
- **Build and stage the shim tree from the operator image.** `libvrocm.so`, `rocm-monitor` and
  `rocm-cumask-check` compiled from `csrc/amd/rocm-slicing-shim/` — the repository's second library built
  from its own sources rather than a pinned upstream commit — installed into `${GPUSTACK_LIB_DIR}/amd/`
  beside an `amd/ld.so.preload` naming the library's container path.
- **Derive a CU mask that is correct on both architectures, and prove it against a fixture rather than
  against a card.** The arithmetic is closed-form integer work over the card's gfx name and four HSA
  agent-info fields, with no probing and no fallback in the allocation path.
  `references/amd-cumask-conformance.md`'s tables A and B are the fixture; the Go must reproduce their
  `mask` column row for row.
- **Fail closed on anything the topology cannot answer.** `binding/hsa` discards individual agent-info
  errors and leaves zero values (`binding/hsa/library.go:112-129`), so an unreadable field arrives as
  `0` rather than as an error — which in a naive derivation is a division by zero, and in a slightly less
  naive one is a mask that confines nothing. An incomplete or unrecognised topology refuses the
  allocation; it never guesses a branch.
- **Give each slice its own CUs while the card has CUs to give.** Disjoint packing is not an
  optimisation here — measured, two identically-placed 50 % masks deliver 25.8 % each and leave half the
  card idle, which is worse than not masking at all. Overlap stays legal (masks may overlap and share
  fairly, which is why `CoresPercentageOvercommit` is `true`) and is what the card degrades into when it
  is oversubscribed, not what it starts from.
- **Keep the reported capacity and the enforced quota one story.** The container's
  `VROCM_DEVICE_MEMORY_LIMIT_<i>`, its `HSA_CU_MASK` `GPU_list` index and its position in
  `ROCR_VISIBLE_DEVICES` are one tuple; changing any one of them alone misaligns the other two.
- **Leave every other mode untouched.** Exclusive, shared and visibility responses, the credit
  accounting, the `.sliced.units` capacity fold and the reclaim loop are unchanged; the regression
  evidence is that `make test` still passes with no edits to their tests.
- Success is testable: (1) `make lint` and `make test` pass, with table-driven cases covering the mask
  derivation, the packing, and the allocator's full response; (2) a locally built `linux/amd64` operator
  image carries `${GPUSTACK_LIB_DIR}/amd/{libvrocm.so,rocm-monitor,rocm-cumask-check,ld.so.preload}` with
  the shim spec's four linkage assertions enforced inside the build; (3) an `arm64` build of the same
  Dockerfile succeeds and leaves that directory with only `ld.so.preload`; (4) on the RDNA host, several
  packed windows at different sizes and offsets run simultaneously and saturated, each container's
  `rocm-cumask-check` exits `0`, and the aggregate throughput shows the windows are physically disjoint.

### Non-Goals

- **Changing anything under `csrc/amd/rocm-slicing-shim/`.** The tree is `Built` and its verification
  suite is green on both architectures. This spec consumes it.
- **Enforcing compute anywhere in our code.** ROCr applies the mask; we compute and inject it. There is
  no launch interception, no token bucket and no tuning surface — the shim spec measured that the
  platform already delivers a hard per-tenant ceiling and fair sharing at zero overhead.
- **Physical slicing / SR-IOV for AMD.** `.partitioned*` is a separate family and a separate effort; a
  consumer part cannot do it at all.
- **Multi-card logical slicing.** A sliced request is one card, pinned by two admission gates that say so
  deliberately (`pkg/worker/webhooks/worker/pod.go:492`, `:669` and `instance.go:873` →
  `validateSingleCardRequest`). This spec aligns with that deferral: the injection loop is written for
  several cards, exactly as the NVIDIA and THead branches are, and adds no card-count guard of its own.
- **Gating the detector on the mask self-check probe.** The shim spec left this as an open product
  question; it is closed by a technical fact rather than decided. `rocm-cumask-check` is compiled by
  `hipcc` and links `libamdhip64` and `libhsa-runtime64` (`csrc/amd/rocm-slicing-shim/build.sh`, the
  `rocm_cumask_check` arm), and the device-manager container carries no ROCm user space — the AMD
  deployment model puts it in the workload container. The probe therefore cannot run where the detector
  runs. It ships mounted into the sliced container instead, which is where a card's mask can actually be
  interrogated. See F3 for what it does and does not answer.
- **An architecture gate on the detector.** ROCm ships no `aarch64` user space, so `libhsa-runtime64` and
  `libamdsmi` do not load on `arm64` and the AMD detector finds no cards there at all. This is a stronger
  argument than THead's (which rests on where a PPU card is sold) and it leaves no residual: there is no
  `arm64` node on which the capability could be advertised without the library. The *derivation*
  nonetheless refuses an unrecognised gfx family at `Allocate` — the capability is advertised per the
  repo's convention, and the honest refusal happens where the topology is actually read.
- **A metrics scraper over the ledger region.** The region's layout is a documented contract and
  `rocm-monitor` already reads it; exposing it as a GPUStack metric is a separate effort with its own
  surface, and `Devices.Status` is the wrong transport for it (it is rebuilt wholesale each reconcile
  from Spec plus Pod annotations, with no live query).
- **Surfacing the minimum-slice floor to admission.** A request below one quantum is refused at
  `Allocate` on **both** architectures; teaching the webhook to refuse it earlier needs a per-card
  granularity field in the API and a validation rule. Recorded as an Open Question with what it would
  cost.
- **A new API field for the CU ledger.** The existing `AllocatedPhysicalPlacements` transport carries the
  windows, gated by allocation mode. See F4 and Alternatives.
- **Turning logical slicing into a security boundary.** It stays cooperative isolation — removing the
  preload restores the card's memory, `env -u HSA_CU_MASK` restores its compute, and
  `HSA_CU_MASK_SKIP_INIT=1` does the latter without removing anything. Same posture as every other
  backend.
- **Virtualising `rocm-smi` / `amd-smi`.** They read sysfs and the DRM nodes rather than HIP, so nothing
  a preload does reaches them. `rocm-monitor` exists because of this.

## Proposal

An AMD GPU advertises logical slicing, and a container that requests a slice of it starts with
`libvrocm.so` preloaded, its VRAM quota in its environment, and a CU mask that this repository derived
and placed.

Five pieces, in the order they gate each other:

1. **The image builds the shim.** One `pack/` stage compiles the checked-in sources inside
   `rocm/dev-ubuntu-22.04:7.2.4` — the same image the verification skill uses — and installs the library
   and the two tools; the final image copies them to `${GPUSTACK_LIB_DIR}/amd/` and installs the
   `ld.so.preload` asset beside the existing four.
2. **The detector advertises the capability.** Per card, `LogicalSliced{Count: 128,
   CoresPercentageOvercommit: true}` — the NVIDIA and THead arm's shape — plus the
   `device.SetGroupSlicedDetails(grpList)` call the AMD detector has never had, without which the group
   aggregate stays zero and the pool is silently un-sliceable.
3. **The mask is derived.** `allocator/amd/cumask.go` turns a card's topology and a requested percentage
   into a mask string by the arithmetic `references/amd-cumask-conformance.md` fixes, selecting the
   branch on the card's gfx architecture family. Topology is read from the HSA agent-info API behind a
   `_linux.go` seam; the arithmetic itself is platform-independent and unit-tested against both
   conformance tables.
4. **The mask is placed.** The allocator reads the card's live CU occupancy — the union of what live
   allocations recorded in their Pod annotations and what in-flight allocations published into their
   reservation, keyed by the accelerator's UUID — and takes the first free window of the required size,
   falling back to the least-overlapped window when none is free.
5. **The allocator hands out the slice.** A `Sliced` server behind `!opts.NoSliced`, whose response keeps
   today's `AMD_VISIBLE_DEVICES` (the container-runtime hook is what injects the device nodes) and adds
   `ROCR_VISIBLE_DEVICES`, the mask, the per-card memory limit, the ledger path and the log level, plus
   the mounts that preload the library and give its usage region a writable directory.

Nothing upstream of `Allocate` changes: the request API, the Pod webhook's `.sliced.*` fold, the
`.sliced.units` capacity key, the Kueue credit accounting and the four-view status are all vendor-blind
and already carry AMD.

### User Stories

#### Story 1

As a platform user running workloads on an AMD GPU node, I want to request a fraction of a card with
`.sliced.memory-percentage` and `.sliced.cores-percentage`, so that several workloads share one card with
predictable capacity instead of taking a whole card each.

#### Story 2

As a cluster administrator, I want an AMD card's logical-slicing capability to show up in the `Devices`
status and in the pool's InstanceType capability, so that the scheduling chain materializes sliced
flavors and queues without me configuring anything per node.

#### Story 3

As a platform user sharing one AMD card, I want my slice to get its own compute units while the card has
units to give, so that two 50 % containers each run at half a card rather than each running at a quarter
while half the card sits idle.

#### Story 4

As a GPUStack Operator maintainer, I want the operator image to build and stage the shim from this
repository's own sources, so that the library a sliced container preloads ships with the operator that
allocates the slice instead of arriving out of band.

#### Story 5

As a user debugging a slice, I want a mounted `rocm-monitor` to print the quota and usage the container
was given, and a mounted `rocm-cumask-check` to tell me whether the compute mask actually took effect, so
that I can tell a throttled workload from one whose isolation silently failed open — `rocm-smi` and
`amd-smi` report neither, and the platform reports nothing at all when it rejects a mask.

### Core Features & Acceptance Criteria

#### F1 — `pack/` build, staging and the preload asset

- **`pack/gpustack-operator/external/amd/build-libvrocm.sh`** — a thin wrapper following
  `external/thead/build-libvppu.sh`: it takes `<src-dir> <out-dir>`, delegates every compile decision to
  `csrc/amd/rocm-slicing-shim/build.sh` (`lib`, then `tool`, with `OUT` pointed at the output directory),
  and installs the three artifacts. It carries **no compile recipe of its own** — the tree's `build.sh`
  is the single place translation-unit lists and flags live, and it says so at the top of the file. It
  clones nothing and takes no `ARG LIB_*_COMMIT`: the source is in-repo.
- **The wrapper asserts what it built, and fails the build otherwise**, by calling the tree's own
  `build.sh check` rather than re-implementing the assertions. That command is the shim spec's F1
  contract: the exported set is exactly the interposed HIP names, `DT_NEEDED` is exactly `libc.so.6`, no
  `GLIBC_` requirement above `GLIBC_2.4`, and no `hip*`/`hsa*` name among the undefined symbols. All four
  are properties of a library that has to load inside a workload container nobody controls; a build that
  silently lost one would ship a library that fails at container start, or one that no longer intercepts.
- **The two tools are recorded, not asserted.** `rocm-monitor` links `libc` alone but is an executable,
  so its startup stub carries `__libc_start_main@GLIBC_2.34` whatever it calls; `rocm-cumask-check` links
  the ROCm runtime by design. The wrapper prints both floors and both `NEEDED` sets and asserts neither —
  the shim spec's case 1 makes the same distinction for the same reason.
- **One `xbuild-amd-rocm` stage, not a per-runtime fan-out.** The product links no ROCm object and one
  build was measured interposing across two ROCm majors, so unlike NVIDIA (`cuda-12`/`cuda-13`) and
  Ascend (`cann-<major>-<family>`) there is one stage and `${GPUSTACK_LIB_DIR}/amd/` is flat.
- **Arch-selected, following the THead idiom verbatim** (`pack/gpustack-operator/Dockerfile:327-339`):
  `xbuild-amd-rocm-amd64` builds the tree inside `rocm/dev-ubuntu-22.04:7.2.4`; `xbuild-amd-rocm-arm64`
  is a `${UBUNTU_IMAGE}` stand-in whose `WORKDIR /out` produces an **empty** directory; and
  `FROM xbuild-amd-rocm-${TARGETARCH} AS xbuild-amd-rocm` selects between them, needing no `ARG
  TARGETARCH` of its own. Not `FROM --platform=linux/amd64`, which would resolve an amd64 manifest under
  emulation on the arm64 leg and ship an unloadable object; not omitting the stage, because the final
  stage's `COPY --from` is unconditional.
- **The final stage** copies `/out` to `${GPUSTACK_LIB_DIR}/amd` beside the existing copies (`:426`) and
  `install -D`s `rootfs/etc/gpustack/lib/amd/ld.so.preload` beside the other four (`:510-517`).
- **`pack/gpustack-operator/rootfs/etc/gpustack/lib/amd/ld.so.preload`** is one line naming the
  **container** path of the library:

  ```
  /usr/local/vrocm/libvrocm.so
  ```

  That path and the allocator's mount constant are one contract and must not drift.
- Acceptance: `make package gpustack-operator` on an amd64 target produces an image where
  `${GPUSTACK_LIB_DIR}/amd/` holds `libvrocm.so`, `rocm-monitor`, `rocm-cumask-check` and
  `ld.so.preload`; the same Dockerfile builds for `linux/arm64` with that directory holding only
  `ld.so.preload`; `rocm-monitor` executes in that image — no ROCm, no device — reaching its own "no
  usage region" message rather than dying in the loader; `build.sh check`'s four assertions run inside
  the build rather than being checked by hand afterwards.

#### F2 — Detector: per-card `Status.LogicalSliced`, and the group fold AMD never had

- In `pkg/devicemanager/detector/amd/device.go`, the per-card `status` block (`:206-209`, which today
  sets only `Unhealthy`) gains `LogicalSliced{Count: 128, CoresPercentageOvercommit: true}`.
- `device.SetGroupSlicedDetails(grpList)` is added before `DetectAccelerator` returns (`:224`). **AMD is
  the only detector of the nine without this call**, and without it the per-card figure never reaches the
  group aggregate the pool capability reads (`SlicedDetail.Logical.Count > 0`), so the capability would
  be set and invisible.
- The figure is the NVIDIA and THead one, deliberately. 128 is a loose device-plugin token pool — the
  binding constraint on a slice request is its memory budget, and the safe direction is generous. The
  CU-atom arithmetic gives a smaller *disjoint* count (10 windows on a 60 CU / 3 SE RDNA card, 38 on a
  304 CU / 8 XCC CDNA one), and that is deliberately **not** what this count reports: overlap is legal,
  so the disjoint count is a packing property, not a capacity ceiling.
- Overcommit is `true` because masks may overlap and tenants sharing an overlap divide it fairly —
  measured on both architectures. Memory is the non-oversubscribable dimension and the flag does not
  touch it.
- **Nothing gates the capability** — not the staged library's presence, not `runtime.GOARCH`, not the
  self-check probe, not the gfx family. The first two follow the NVIDIA and THead arms; the third is a
  Non-Goal with the technical reason it cannot be otherwise; the fourth belongs at `Allocate`, where the
  topology is read and a refusal can carry an actionable message.
- Acceptance: no new test, for the reason the THead arm carries none — the addition is a two-field struct
  literal inside `DetectAccelerator`, which needs a real card to reach, and the aggregate it feeds is
  covered where it lives, in `pkg/device`. `make test` still passing is the claim; the AMD detector's
  coverage figure moving is reported rather than hidden behind a test that restates a literal.

#### F3 — The CU-mask derivation, and what the probe is for

**Three jobs are easy to read as one, and only one of them is code this feature writes.** The shim spec
and `csrc/amd/rocm-slicing-shim/README.md` both open with this table because the design keeps being
re-read as "ask the card what mask to use", which is exactly what it does not do:

| | job | who does it | when |
| --- | --- | --- | --- |
| **Compute** | topology + a requested percentage → a mask string, by closed-form arithmetic | this feature — `allocator/amd/cumask.go` | per allocation |
| **Inject** | emit `HSA_CU_MASK` into the container beside `ROCR_VISIBLE_DEVICES` and the memory limit | F5 — the device-plugin `Allocate` | per allocation |
| **Enforce** | apply the mask to the workload's queues | **ROCr**, at its own initialisation — never our code | per process |
| **Check** | run a kernel, read the hardware back, decide whether the mask took effect | `tools/rocm-cumask-check` | by hand, in the sliced container |

**The mask is derived, never discovered.** The inputs are the card's gfx name and four HSA agent-info
fields — `COMPUTE_UNIT_COUNT` (`0xA002`), `NUM_SHADER_ENGINES` (`0xA00C`), `NUM_SHADER_ARRAYS_PER_SE`
(`0xA00D`) and `NUM_XCC` (`0xA111`), all four already present as constants in
`binding/hsa/const.go:630-657`. There is no probing, no trial launch, no measurement in the loop and no
fallback: 60 CU / 3 SE / 1 XCC at 50 % is `0:0-29` and nothing else. That is what makes the derivation
testable against a table rather than against a card.

**The probe answers a different question, and it is not optional that somebody can ask it.** Deriving
correctly and being obeyed are not the same thing, and the platform reports neither: a mask ROCr rejects
produces no error, no log line and no changed return code. `0:0-14` looks like fifteen CUs and delivers
sixty; on a multi-XCC card `0:0` reads as a plausible 3.7 % of throughput while the container reaches 267
of the card's 304 CUs. So `rocm-cumask-check` launches its own kernel — eight-way oversubscribed and
repeated, so a unit the mask enabled but no workgroup happened to land on cannot read as a broken mask —
reads `HW_ID` and, on multi-XCC parts, `XCC_ID` from inside each wave, and unions the physical slots its
waves actually ran on. It judges by **occupancy, not throughput**, because on CDNA throughput cannot tell
the three cases apart. Exit codes: `0` the mask took effect as asked, `1` it did not, `2` the probe could
not run.

**It has two modes, and this design uses the second one.**

```bash
rocm-cumask-check --percent 25          # no mask in env: derive one, setenv, re-exec, then verify
HSA_CU_MASK=0:12-23 rocm-cumask-check   # a mask already in env: verify that one, as it stands
```

The first mode carries a copy of the derivation, which is a **reference implementation, not the
production path** — and its `derive` arm always places the window at zero (`"%u:0-%u"`), so it cannot
produce or check the offset windows F4's packing emits. The second mode parses whatever mask string is in
the environment, builds the expectation from it (unit count, WGP wholeness on RDNA, the per-XCC partition
and full XCC coverage on CDNA) and compares that against measured occupancy. **That is the mode the
sliced container is in**, because the allocator has already put `HSA_CU_MASK` in its environment.

So the probe appears in exactly three places in this spec, none of them in the allocation path:

- **A judge in F6.** Our Go emits packed masks; the probe, in mode two, decides on real hardware whether
  each confined the container the way the derivation claimed. It is necessary but **not sufficient** —
  on RDNA it compares occupancy *cardinality*, not physical identity, because the bit→physical-WGP
  mapping was never measured, so two different packed masks could each pass while aliasing the same
  units. F6 pairs it with simultaneous saturated runs, which is what closes that gap.
- **A diagnostic mounted into the sliced container** (F5). It is the only thing on the node that can
  answer "is my neighbour noisy, or did my isolation silently fail open" — `rocm-smi` and `amd-smi` read
  sysfs and never see a mask at all.
- **Not in the detector** (Non-Goals). It needs a ROCm user space the device-manager container does not
  have.

**The branch is selected by architecture family, and that is the correction that matters most.** An
`NUM_XCC > 1` discriminator is wrong, not merely imprecise: `gfx90a` — MI210, and each GCD of an
MI250X — is CDNA2 silicon reporting **one XCC**, and it would take the RDNA branch and be given WGP
pairing that its architecture does not have. The shim tree compiles device code for it explicitly
(`csrc/amd/rocm-slicing-shim/build.sh:55`, `OFFLOAD_ARCH` begins `gfx90a`), so it is a part this product
expects to meet.

```
# --- family selection ------------------------------------------------------
gfx9*                -> CDNA/GCN arithmetic, X = NUM_XCC (0 or absent -> 1)
gfx10* gfx11* gfx12* -> RDNA arithmetic
anything else        -> REFUSE to derive (the allocation fails with the family named)

# --- validation, before either branch --------------------------------------
REFUSE unless CU > 0, and (RDNA) SE > 0 and CU is even, and the quantum divides into CU
# binding/hsa leaves an unreadable agent-info field as 0, so "absent" and "zero" are the same
# value here; both must fail closed rather than divide.

# --- RDNA branch -----------------------------------------------------------
W = CU / 2                          # WGP count; 60 CU -> 30 WGP
n = round(W * pct / 100)            # requested WGPs
n = floor(n / S) * S                # align DOWN to a multiple of the shader-engine count
REFUSE when n == 0                  # below one round: see below
n = min(n, W)
window length in CU = 2n            # quantum Q = 2S CU

# --- CDNA/GCN branch -------------------------------------------------------
n = round(CU * pct / 100)           # requested CUs
n = floor(n / X) * X                # align DOWN to whole "one CU in every XCC" atoms
REFUSE when n == 0                  # a sub-atom mask does NOT clamp -- it fails open
window length in CU = n             # quantum Q = X CU
```

Emission is `"<i>:<lo>-<hi>"`, `<i>` the card's position in `ROCR_VISIBLE_DEVICES` (F5's tuple rule),
`<lo>` the placed window's first CU bit and `<hi>` its last.

- **The two arithmetics share nothing and neither degrades safely into the other.** Carrying RDNA's
  pairing onto CDNA does not fail, it doubles every slice; carrying CDNA's atom onto RDNA splits WGP
  pairs, and a split pair makes ROCr discard the whole mask and hand back the card.
- **A sub-quantum request is refused on both architectures.** On CDNA this is forced — a sub-atom mask
  leaves the XCCs it never mentions running unmasked. On RDNA the C reference implementation clamps *up*
  to one shader-engine round instead, and this Go deliberately **diverges from it there**: a 1 % request
  on a 60 CU / 3 SE card would receive a 10 % compute ceiling while Kueue charges 1 %, and an accounting
  mismatch that silently favours the tenant is not better than a refusal that names the number. No
  conformance-table row changes — table A's smallest row is 10 %, and it never tested below it, so its
  lack of a reject row is not evidence of intent. The refusal message names the card's minimum
  percentage, which is the actionable half.
- **`gfx90a` takes the CDNA branch with `X = 1`, which makes its atom one CU** — a contiguous window of
  any length, with no pairing rule and no XCC-coverage rule to violate. That is the least-assuming of the
  three behaviours, and it is the reason this routing is safe to ship unmeasured: the CDNA fail-open mode
  (an XCC receiving no bit) cannot occur when there is one XCC, and the RDNA fail-open mode (a split
  pair) is not a rule on this silicon. What is unverified is whether the shader-engine round-robin costs
  a remainder there; that would cost throughput, never isolation. Carried as an Open Question.
- **Topology reading is a seam, the arithmetic is not.** `cumask.go` holds pure integer functions over a
  topology struct and compiles and tests everywhere, including darwin; `cumask_driver_linux.go` reads the
  gfx name and the four fields through `binding/hsa` and `cumask_driver_other.go` stubs it — the shape
  `allocator/{nvidia,thead}/mig_driver_{linux,other}.go` already uses, and the reason it exists is that
  `pkg/deviceplugin` links the stdlib `plugin` package while a cgo binding pulls in `runtime/cgo`, a
  combination that aborts a darwin test binary at load.
- **`binding/hsa` gains three fields and their getters.** `AgentProperty` (`binding/hsa/library.go:83-90`)
  carries `ComputeUnitCount` and `Name` (the gfx string) today; `NumShaderEngines`,
  `NumShaderArraysPerSE` and `NumXcc` are added beside them, read in the same `GetAgents` iteration
  through the constants that already exist. Topology comes from this API and never from KFD sysfs — both
  agree on both architectures, but sysfs paths are not an interface AMD maintains, and on a card running
  under SR-IOV `rocm-smi` could not complete a libdrm query at all while agent-info returned every field.
- Acceptance: table-driven cases reproduce every row of conformance tables A and B — the `mask` column
  for the valid rows, the refusal for table B's 1 % row — plus a refusal for RDNA below one round; the
  degenerate inputs all refuse rather than panic (`CU=0`, odd `CU`, `SE=0`, `SE > CU/2`, `XCC=0`,
  `XCC > CU`, `CU` not divisible by the quantum, an unrecognised gfx name); `gfx90a` routes to the CDNA
  branch with `X=1`; and `multiProcessorCount` appears nowhere in the code (it means WGPs on RDNA and CUs
  on CDNA, and mixing it with the topology API is silently out by 2× on one of them).

#### F4 — The per-card CU ledger, and the packing that rides it

**A mask is a position, not only a size.** Two containers handed the same window share it rather than the
card: measured on RDNA, two 50 % tenants on the same half deliver 25.8 % each and aggregate to 51.5 %,
while a disjoint pair delivers 50.4 % each and aggregates to 100.9 %. On CDNA the same reading is worse
because it is invisible — two tenants given the "obviously disjoint" bit ranges `0:0-3` and `0:4-7` were
each measured occupying 156 CUs and overlapping on 152 of them, with healthy-looking throughput
throughout. So the allocator must know what a card already carries before it places anything.

- **The placement decision happens inside the node allocation mutex, and this is the load-bearing
  structural fact.** `Allocate` holds the mutex across identify / check / reserve
  (`pkg/deviceplugin/server.go:797-816`), reads placement occupancy there for the partitioned mode only
  (`:827-838`), publishes the reservation at `:987-1003`, releases the mutex, patches the Pod at
  `:1036-1049`, and only then calls the vendor's `GetContainerAllocateResponse` (`:1051-1062`). A window
  chosen in that last call would be chosen outside the mutex — two concurrent allocations would both
  leave with placement-less reservations and both first-fit onto the same window — and it would be
  chosen *after* the durable annotation was already written, so nothing would survive a restart.
- **The vendor hook is therefore a new optional interface, mirroring `PhysicalSlicedResponder`:**

  ```go
  LogicalSlicedResponder interface {
      // Chosen under the node mutex: pure arithmetic over the occupancy snapshot, no I/O.
      PlaceLogicalSliced(ctx, pod, ctr, devs, allocated, occupied) (map[Resource][]Placement, error)
      // Built outside it, from the windows the server has already published.
      GetLogicalSlicedResponse(ctx, pod, ctr, devs, allocated, placements) (*ContainerAllocateResponse, error)
  }
  ```

  Two methods rather than one so that the responder's own I/O — `osx.MkdirAll` for the ledger
  directory — stays out of the serialized section, which is the discipline `server.go:1036` already
  states for the annotation patch. There is **no `Rollback`**, unlike the physical interface: a mask is a
  string and nothing was materialized on the card. A responder that does not implement it behaves exactly
  as today.
- **The transport is the existing `AllocatedPhysicalPlacements` field, gated by mode.** No new API field,
  no `make generate`, no CRD surface. The physical accumulator already skips an entry whose
  `AllocatedPhysicalProfile` is empty (`pkg/deviceplugin/controller.go:953`), so a logical entry — which
  writes intervals and leaves the profile empty — cannot pollute MIG accounting; the logical accumulator
  gates on the opposite (`Mode == Sliced` **and** an empty profile). The one other reader,
  `priorPartitionTokens` (`server.go:1149-1163`), does not check the profile, but it is reachable only
  from the `profile != ""` branch, so a container would have to be both sliced and partitioned to meet
  it — asserted by a test rather than left to reasoning.
- **The intervals are CU bits on both architectures**, i.e. exactly what appears in `HSA_CU_MASK`. The
  branch difference lives in the quantum, not in the representation. The field's own documentation gains
  a sentence saying which unit applies in which mode.
- **The logical occupancy is keyed by the accelerator's UUID, not by `(Group, Device)`.** The physical
  ledger keys by both (`pkg/deviceplugin/helper.go:33-41`), and AMD's group ID is derived from the
  detected name and memory (`detector/amd/device.go:168-201`) — so a re-detect that regroups a card
  (a changed marketing name, a VRAM figure that reads differently) leaves the old annotation keyed to a
  group that no longer exists, the window is forgotten, and the same physical card hands out its first
  window twice. The UUID is the identity that survives regrouping.
- **Windows are quantised, and start and length share the quantum.** `Q = 2S` CU on RDNA (one full
  round-robin round of WGP pairs) and `Q = NUM_XCC` CU on CDNA; both a window's start and its length are
  multiples of `Q`. Length is a multiple by construction in both branches, so quantising the start makes
  windows tile: a 60 CU / 3 SE card has 10 slots of 6 CU, a 304 CU / 8 XCC card has 38 slots of 8 CU.
  On CDNA the start quantum is what preserves full XCC coverage at any offset (bit `i` lands on XCC
  `i mod X`); on RDNA it keeps a window from splitting a WGP pair, which discards the entire mask.
- **Placement is first-fit, then least-overlap over a merged interval set.** Take the lowest-indexed free
  window of the required length; if none is free, take the quantised start whose window overlaps the
  fewest covered CUs, ties broken by lowest index. **The merge is not cosmetic:** the occupancy union
  appends reservation intervals to annotation intervals (`server.go:1189-1199`), so a live allocation
  normally appears **twice** — harmless for the physical ledger's binary overlap test, and a systematic
  bias for anything that counts. Merging before measuring removes it. What merging cannot preserve is
  tenant multiplicity, so an already twice-shared window and a once-shared one look alike; that is
  accepted for a fallback that only runs on a full card.
- **A retried `Allocate` reuses the container's own prior window.** The kubelet re-runs `Allocate` for a
  container whose checkpoint it lost, and by then this container's window is part of the node's
  occupancy — deciding afresh would read it as somebody else's and move the container to a different
  window while stranding the first. The partition path already solves exactly this
  (`server.go:896-918`, `priorPartitionAllocation`); the logical path reuses the same lookup.
- **The response is built before the durable patch, which also closes a pre-existing hole.** Today a
  responder error at `server.go:1058-1062` returns without releasing the reservation and without undoing
  the annotation patch made at `:1042`, so a Pod that never started keeps its allocation until the Pod
  object disappears — a hole that already affects the NVIDIA and THead sliced responders, whose
  `MkdirAll` and memory derivation can both fail. Reordering to build-then-patch, with the reservation
  released on a build failure, fixes it for every vendor at once. The existing patch-failure path is
  already correct (`:1036-1048`) and is left alone.
- **The ledger is advisory in exactly one direction, and that is stated rather than glossed.** It cannot
  stop a card from being oversubscribed — `CoresPercentageOvercommit` is `true` and admission counts
  credits, not CU windows — so a full card degrades into overlap rather than into a refusal. What the
  ledger buys is that the degradation starts only when the card is actually full.
- Acceptance: table-driven cases over the packing — an empty card places at 0; a card carrying one window
  places the next one after it; a freed window in the middle is reused ahead of the tail (first-fit, not
  next-fit); an oversubscribed card picks the least-overlapped start deterministically, and the same
  allocation appearing in both occupancy sources does not bias that choice; every emitted window
  satisfies its branch's start and length quantum; a retry reuses the identical window; a responder
  failure releases the reservation and leaves no annotation; and a sliced allocation's intervals are
  invisible to both the physical accumulator and the partition retry path.

#### F5 — Allocator: the `Sliced` server and its injection

- `New` registers `newServer(logger, DeviceAllocationModeSliced)` behind `!opts.NoSliced`, between the
  shared and visibility servers, matching the ordering the other vendors use.
- `GetContainerAllocateResponse` stops discarding its Pod and container parameters (`:98-99`) and keeps
  its single pass over the allocated cards, now collecting the `(group, accelerator)` pairs the sliced
  path needs. The non-sliced response is byte-for-byte what it is today; the sliced one is served by
  F4's two-method interface instead.
- **Environment**, the whole of it:

  | | |
  |---|---|
  | `AMD_VISIBLE_DEVICES` | unchanged from today's response — the container-runtime hook reads it to inject `/dev/kfd` and the card's render node. It is **not** read by the ROCm user-space runtime; setting it alone leaves `hipGetDeviceCount` unchanged |
  | `ROCR_VISIBLE_DEVICES` | the ROCr-level visibility and, more importantly, the **ordering** that defines the index space the next two variables live in. **Its value format is a prerequisite, not an acceptance item** — see below |
  | `HSA_CU_MASK` | `"<i>:<lo>-<hi>"` per allocated card, `<i>` the card's position in the `ROCR_VISIBLE_DEVICES` list; the window from F3 and F4 |
  | `VROCM_DEVICE_MEMORY_LIMIT_<i>` | MiB, bare integer (no unit suffix — the shim parses a bare MiB integer, unlike HAMi-core), from `.sliced.memory-percentage` / `.sliced.memory-mib` against **that card's own group** VRAM |
  | `VROCM_LEDGER_PATH` | `/var/run/vrocm/ledger` — the region file inside the per-container rw mount below |
  | `LIBVROCM_LOG_LEVEL` | `1` — denials and errors — injected only when the workload declares no value of its own (`deviceplugin.ContainerEnvDeclared`) |

- **The device identity in `ROCR_VISIBLE_DEVICES` is unresolved and blocks the tuple.** The only measured
  case is numeric (`ROCR_VISIBLE_DEVICES=1,0` reorders, so `0:` addresses physical card 1); UUID
  acceptance was never tested. Neither candidate is free: an index is a position in ROCr's own
  enumeration, and this repository has not shown that `Accelerator.Index` — an AMD-SMI detection-loop
  counter (`detector/amd/device.go:102-115`) — equals it; while the UUID form depends on
  `AsicInfo.GetUniqueId()`, which returns the **empty string** when the ASIC serial reads `N/A`
  (`binding/amdsmi/library_device.go:85-90`). If the hook exposes one identity while this variable names
  another, the container sees zero HIP devices, or fails initialisation, or — worst — caps and masks a
  *different card* than the one it was given. T1 settles the form on hardware before the allocator is
  written, and an ambiguous or absent mapping fails the allocation rather than guessing.
- **The three device-scoped variables are one tuple.** `HSA_CU_MASK`'s `GPU_list` index and
  `VROCM_DEVICE_MEMORY_LIMIT_<i>`'s `<i>` are both positions in the **post-`ROCR_VISIBLE_DEVICES`**
  ordering, not physical ordinals — ROCr has already applied that variable by the time agents are
  enumerated. Numbering everything by position in the emitted list makes them agree automatically;
  changing any one alone misaligns the other two. `<i>` is a loop position and can only ever be `0`
  today, because admission pins a logical slice to one card; the loop is written for several anyway,
  exactly as the NVIDIA and THead branches are.
- **`HSA_CU_MASK`'s `GPU_list` must be a decimal index.** A `GPU-<hex>` UUID there is discarded outright
  — the whole segment, silently, on both architectures. `ROCR_VISIBLE_DEVICES` is a different variable
  with a different grammar and this constraint does not transfer to it.
- **The compute figure is emitted even at 100 %**, for the tuple's sake and because a whole-card mask is
  a real statement about what this container may reach. The measured cost is a full-width `0:0-303` mask
  reading 566.8 TFLOP/s against an unmasked 573.3 on `gfx942` — 98.9 %, i.e. about 1.1 % — and none
  measurable on RDNA. Recorded rather than optimised away.
- **`LIBVROCM_LOG_LEVEL=1` states the level rather than changing behaviour**: it is already the library's
  own default, and naming it keeps the level a property of the allocation instead of a library default a
  later shim change could move underneath it. Level 1 is per-*denial*, not per-call — the one line that
  answers "why was my allocation refused" — which is why it is `1` and not the `0` HAMi-core needs.
- **Mounts**, all container-scoped:

  | Container path | Host path | Mode |
  |---|---|---|
  | `/etc/ld.so.preload` | `<OperatorLibDir>/amd/ld.so.preload` | ro |
  | `/usr/local/vrocm/libvrocm.so` | `<OperatorLibDir>/amd/libvrocm.so` | ro |
  | `/usr/local/vrocm/rocm-monitor` | `<OperatorLibDir>/amd/rocm-monitor` | ro |
  | `/usr/local/vrocm/rocm-cumask-check` | `<OperatorLibDir>/amd/rocm-cumask-check` | ro |
  | `/var/run/vrocm` | `<PodWorkDir>/run/vrocm` | rw |

  The two tools ride the library's own mount, as Ascend mounts `enpu-monitor` and THead mounts
  `ppu-monitor`. The rw directory lives under the pod work dir so the existing per-pod GC reclaims it,
  and it is **per container** on purpose: the region is addressed by container-local card index, so a
  node-wide location would let two containers' index `0` — two different physical cards — charge one
  slot. The shim's default of `/dev/shm/vrocm-ledger` is a convenience for a hand-run; the `.sliced` path
  must always set the variable, because `/dev/shm` is container-private only until `hostIPC: true` or an
  `emptyDir{medium: Memory}` makes it shared.
- **No card-count guard is added**, which is the NVIDIA and THead choice: admission already pins a slice
  to one card, and re-checking it here duplicates a gate that lives upstream.
- Acceptance: table-driven cases assert the full response for one card — every environment key and every
  mount — and a two-card case that pins the loop's shape the way NVIDIA's
  `TestGetSlicedContainerAllocateResponse_MultiCard` does, named so it is clear admission does not admit
  it today; the mask for a 25 % request on a 60 CU / 3 SE card is `0:0-11` and not `0:0-14`; a second
  25 % request on the same card is `0:12-23`; a sub-quantum request on either architecture is refused
  with a message naming the card's minimum percentage; `LIBVROCM_LOG_LEVEL` is injected when absent and
  left alone when the container declares it; and the exclusive / shared / visibility responses are
  unchanged.

#### F6 — Verification on the RDNA host

The mechanism is already proven on hardware: the shim spec's eight gates and the verification skill's
seven AMD cases are green on an RDNA host and on a CDNA one, covering memory enforcement across thirteen
allocation families, the reported-capacity surface, every fail-open mask construction, compute quota
semantics and the ledger's lifecycle. What this spec adds on top is Go that composes figures, paths and
mounts — and three things in that composition that no existing measurement covers:

1. **The device identity `ROCR_VISIBLE_DEVICES` accepts** — index or UUID — and whether it composes with
   the container-runtime hook's own restriction. This is a **prerequisite** (T1), settled before the
   allocator is written, because F5's whole tuple is expressed in the index space it defines.
2. **The index alignment.** That the card `HSA_CU_MASK`'s `0:` addresses and the card
   `VROCM_DEVICE_MEMORY_LIMIT_0` caps are the same card, under the emitted `ROCR_VISIBLE_DEVICES`, on a
   **non-zero** physical card — the case a single-card test can never distinguish.
3. **Packed placement at non-zero offsets.** Every conformance-table row places its window at zero, and
   the C reference implementation cannot emit anything else. Offset windows have exactly two measured
   witnesses on RDNA (`0:2-15` and `0:16-29`, both correctly throttled) and one on CDNA (`0:8-15`,
   disjoint from `0:0-7`) — enough to believe the design and not enough to ship it unverified. Whether a
   window's start must be shader-engine-aligned on RDNA, which F4 assumes conservatively, is decided
   here.

- **The run, and why occupancy alone is not the judge.** `rocm-cumask-check` compares occupancy
  *cardinality* on RDNA, not physical identity, because the bit→physical-WGP mapping was never measured.
  Two packed masks could therefore each report the right count while aliasing the same units. So the run
  is **several packer-generated windows of different sizes and offsets, running simultaneously and
  saturated**: 25/25, 50/50, 25/50/25, and one oversubscribed set. Per container, `rocm-cumask-check`
  exits `0` and `rocm-monitor` reports the injected quota; across the set, the aggregate throughput is
  what proves the windows are physically disjoint (a disjoint pair aggregates to ~100 % of a solo card;
  an aliased pair cannot). A solo run per window size supplies the ceiling the concurrent numbers are
  read against — measured on RDNA, a correct partition, a broken one and no partition at all produce
  indistinguishable *concurrent* readings.
- **The fallback is named rather than silently taken.** If the RDNA host cannot host a cluster, the run
  degrades to applying by hand the exact environment and mount set the allocator composes (captured from
  the unit tests' golden response). That still closes all three items; what it does not cover is the
  mount set as the kubelet applies it, and a report taking this path says so.
- **CDNA stays unverified through the allocator, and that is stated as a limit of this spec.** The CDNA
  host available is a rented instance that is itself a container; the branch's arithmetic is covered by
  table B's unit tests and by the probe on the node, but no run of this feature's Go composes an
  injection there. `gfx90a` — the single-XCC CDNA part the family routing exists for — was not available
  at all.
- Acceptance: a recorded PASS/FAIL per item with captured output, and a FAIL is a finding that changes
  the spec rather than a silent retry. The simultaneous-disjointness aggregate and the
  `rocm-cumask-check` exit code are the two that gate the feature.

#### F7 — Documentation

- `README.md`'s accelerator matrix marks AMD logical slicing supported (`:31`), and the note under it
  stays accurate about what the compute budget means here — a **compute ceiling, not a compute QoS**: a
  CU mask carries no memory-bandwidth isolation at all, measured, so a bandwidth-saturating neighbour on
  a disjoint half still costs a tenant a further 25 %.
- `docs/architecture/discovery.md`: AMD joins the preload-library row of the per-vendor mechanism table
  (`:246-248`); the paragraph naming `libvgpu.so` / `libvruntime.so` / the PPU pair gains `libvrocm.so`
  and says what is different about it — the compute dimension is not a variable the library reads but a
  CU mask the operator derives and ROCr enforces, and the memory variables are
  `VROCM_DEVICE_MEMORY_LIMIT_*`; the quiet-logging paragraph gains `LIBVROCM_LOG_LEVEL=1` with the same
  per-denial reasoning THead's carries; and "Where the preload libraries come from" (`:321-340`) records
  the second library built from this repository's own `csrc/` tree, that it needs one stage rather than a
  per-runtime fan-out because it links no ROCm object, and that it is amd64-only because ROCm ships no
  aarch64 user space.
- A short subsection on the fail-open property, because it is the one thing an operator of an AMD node
  must know that no other vendor's page needs to say: a rejected CU mask is silent, and
  `rocm-cumask-check` inside the container is how to tell. It also names the minimum requestable
  percentage as a per-card property, since a refused allocation is the user-visible consequence.
- Routed through the `gpustack-operator-docs` skill so the index, links and tables of contents are
  checked rather than assumed.

### Notes / Constraints / Caveats

- **Everything upstream of `Allocate` is vendor-blind and already carries AMD**: the `.sliced.*` resource
  names derive from the manufacturer table, `SlicedCoresPercent` / `SlicedMemoryMib` are shared helpers,
  and pool capability is `SlicedDetail.Logical.Count > 0`. A diff touching those means the design
  drifted.
- **`make generate` is *not* needed**, because the CU ledger reuses the existing placement transport
  rather than adding a field. That was a deliberate trade — see Alternatives — and it removes the whole
  CRD/protobuf surface from this change.
- **`binding/hsa` turns an unreadable agent-info field into a zero, not an error**
  (`binding/hsa/library.go:112-129`). Every consumer of a topology figure in this feature must treat
  `0` as "unknown" and fail closed; a derivation that divides by it panics, and one that defaults it
  silently emits a mask that confines nothing.
- **The build image is chosen for its glibc, not its ROCm version.** The product is ROCm-agnostic, so the
  base tag controls only the floor — and with the shim's `.symver` pins the floor is held by source
  rather than by the tag. `rocm/dev-ubuntu-22.04:7.2.4` is used because it is what the verification skill
  already pins, so a bump moves one version in two places rather than two versions in two places.
- **That image is large and the amd64 CI leg will pull it.** The stage is a leaf that one `COPY` depends
  on and the registry cache the image workflow already writes covers repeat builds — the same trade the
  THead SDK image made. If it becomes a problem the mitigation is mirroring the image, not dropping the
  stage.
- **`rocm-cumask-check` is the reason the stage needs a full devel image** rather than headers: it is
  compiled by `hipcc`, carries device code for five GPU architectures, and links `libhsa-runtime64`. The
  library itself needs only HIP headers and links nothing. Dropping the probe would not remove the ROCm
  base, only the `hipcc` invocation, which is why it is shipped: the incremental cost is one compile and
  the thing it buys is the only in-container answer to a silent fail-open.
- **`hipDeviceProp_t.multiProcessorCount` is not read anywhere.** It reports WGPs on RDNA (30 on a 60-CU
  card) and CUs on CDNA (304 on a 304-CU card), so a derivation mixing it with the topology API is
  silently out by 2× on one architecture and correct on the other — the worst way to be wrong. The
  conformance reference records it; this code does not touch it.
- **The reported shader-engine count is device-wide and already includes the XCC multiplier.** A card
  reporting `NUM_SHADER_ENGINES = 32` with `NUM_XCC = 8` has four per XCC. Every per-XCC quantity is
  obtained by dividing.
- **Mounting `/etc/ld.so.preload` replaces whatever the workload image had there.** Inherited from the
  three existing preload branches and unchanged here.
- **Preload failure is silent and fails open, and that is the loader's property, not this design's.** A
  missing or non-ELF file produces one `ERROR: ld.so: … ignored.` line and the process runs
  unconstrained; under musl the mechanism is ignored entirely. The ROCm user space a sliced workload
  needs is glibc-only, which makes the second case close to moot, but validating the workload image is
  not something a library that is not running can do.
- **The AMD deployment model is ROCm's, not NVIDIA's**: the runtime lives in the workload container and
  the host passes `/dev/kfd` and the DRM render nodes. So `${GPUSTACK_LIB_DIR}/amd/` is flat, there is no
  host library injection, and there is no `runtimeClassName` — but unlike THead there **is** a
  container-runtime hook, which is why `AMD_VISIBLE_DEVICES` stays in the response.
- **Local iteration**: masking the `COPY --from=xbuild-*` lines for the vendors a change does not touch
  cuts a package build from ~20 GB of pulls to minutes; the AMD stage is the one that must stay unmasked
  here, and the Dockerfile must be restored before committing.

### Boundaries

- **Always:** select the derivation branch on the gfx architecture family; validate the topology and fail
  closed on anything it cannot answer; emit `ROCR_VISIBLE_DEVICES`, `HSA_CU_MASK` and
  `VROCM_DEVICE_MEMORY_LIMIT_<i>` as one tuple indexed by position in the ROCr list; key the logical
  occupancy by accelerator UUID; choose the window under the node allocation mutex and publish it before
  the reservation; keep every preload container-scoped; keep the ledger region per container; assert the
  library's linkage inside the build; keep a card's logical and physical capabilities mutually exclusive.
- **Ask first:** anything that runs on, deploys to, or consumes a card on the AMD hosts; publishing any
  image; changing the `ld.so.preload` entry or the container mount paths; adding a compile recipe
  anywhere other than the shim tree's own `build.sh`; changing the ledger's on-disk layout; raising the
  library's `GLIBC_2.4` floor.
- **Never:** branch the derivation on `NUM_XCC` alone; clamp a sub-quantum request into one on either
  architecture; emit a CU mask that splits a WGP pair on RDNA; emit one that leaves an XCC uncovered on
  CDNA; judge a mask by throughput on multi-XCC hardware; derive shader-engine count from KFD sysfs;
  treat a reported shader-engine count as per-XCC without dividing by `NUM_XCC`; derive the compute quota
  from `multiProcessorCount`; put our variables in the `HIP_` / `HSA_` / `ROCR_` namespaces; default the
  ledger path to a shared host location; duplicate an admission gate inside the allocator; change
  anything under `csrc/`; ship an amd64 shared object inside the arm64 image; describe this mechanism as
  a security boundary.

### Risks and Mitigations

- **`gfx90a` and any other single-XCC CDNA part run arithmetic no conformance row covers** → the family
  routing sends them to the CDNA branch with `X = 1`, whose atom is one CU and which therefore asserts
  neither of the two fail-open rules (no XCC can go uncovered when there is one; no pair can be split on
  silicon without pairs). The residual is a possible shader-engine remainder that costs throughput, never
  isolation, and `rocm-cumask-check` in the container is what would surface it. Carried as an Open
  Question with the experiment named.
- **An unreadable topology field arrives as `0` and produces a mask that confines nothing** → validation
  refuses before either branch runs, and the degenerate inputs are unit cases rather than reasoning.
- **A mis-derived mask silently hands over the whole card** → the conformance tables are the unit-test
  fixture rather than prose, both branches' negative constructions are cases, and F6 puts
  `rocm-cumask-check` on the emitted masks on real hardware. The probe is mounted into every sliced
  container so the same question can be asked again later, on a node nobody is watching.
- **Occupancy cardinality passes while two windows alias the same physical units** → the probe cannot
  decide physical identity on RDNA, so F6 does not ask it to: simultaneous saturated runs across several
  sizes and offsets, judged on aggregate throughput, are what separate disjoint from aliased.
- **`ROCR_VISIBLE_DEVICES` needs an identity the allocator cannot compose** — an index it cannot predict,
  or a UUID that is empty when the ASIC serial reads `N/A` → this is the highest-uncertainty item in the
  contract, which is why it is a prerequisite task rather than an acceptance row. If neither form
  composes, the fallback is to emit nothing and rely on the runtime hook having restricted the container
  to one card, so index `0` is that card by construction — weaker, because it assumes a correctly
  configured hook, and it would be recorded as a node constraint rather than hidden.
- **A window chosen outside the node mutex is chosen twice** → the placement is a mutex-scoped step of
  the generic server, published into the reservation before it unlocks, exactly as the partition
  selection is.
- **The occupancy union double-counts an allocation that has both a reservation and a visible
  annotation** → intervals are merged before overlap is measured, which is also why the fallback metric
  is covered CUs rather than interval count.
- **A re-detect regroups a card and orphans its ledger entry** → the logical occupancy is keyed by
  accelerator UUID, which survives a group ID derived from name and memory changing underneath it.
- **A retry moves the container to a different window and strands the first** → the container's own prior
  placement is reused, mirroring `priorPartitionAllocation`.
- **A responder failure after the durable patch strands a window on a container that never started** →
  the response is built before the patch and the reservation is released if it fails; this also closes
  the same pre-existing hole for the NVIDIA and THead sliced paths.
- **A lost or unreadable allocation annotation is worse than degraded sharing** → an unreadable record is
  skipped by reconciliation and its cards read as free (`pkg/deviceplugin/controller.go:180-192`,
  `:1023-1029`), so after a device-manager restart an exclusive or partitioned allocation could land on a
  card a sliced container still holds. That is a cross-mode ownership violation, not a packing question,
  and it is a **pre-existing property of the allocation model** that this feature inherits rather than
  introduces. Recorded here so it is not mistaken for graceful oversubscription; fixing it is a separate
  effort.
- **A Pod stuck `Terminating` strands its window, and a force-deleted Pod frees a window whose process is
  still running** → inherited from the annotation lifetime the whole ledger rests on
  (`controller.go:173-178`), documented and covered by an integration case rather than solved here.
- **A CU mask carries no memory-bandwidth isolation** — measured, a bandwidth-saturating neighbour on a
  disjoint half costs a compute tenant a further 25 %, and half the CUs already reach 97.7 % of the
  card's bandwidth → not mitigable by any derivation, so it is a documentation obligation: `.sliced`
  is a compute *ceiling*, never a compute QoS. F7 owns the wording.
- **A request below one quantum is admitted and then refused at `Allocate`**, leaving a Pod that cannot
  start → the refusal message names the card's minimum percentage, which is the actionable half; teaching
  admission the floor is an Open Question with its cost. Refusing on both architectures at least makes
  the behaviour one rule rather than two.
- **The graph memory-allocation node bypasses the memory quota** — `hipGraphAddMemAllocNode` was measured
  taking 512 MiB under a 64 MiB quota → inherited from the shim, recorded there with its measurement, and
  not re-litigated here. A container using HIP graph memory nodes can exceed its memory quota.
- **The ROCm devel image pull makes every CI image build slower** → amd64 leg only, leaf stage, registry
  cache; mirror before dropping.
- **A capability change never reaches the `Devices` status**, because the re-detect trigger ignores
  slicing capability and the align path can discard an updated group → nothing this spec adds can change
  between passes: with no gate to read, an AMD card's logical capability is a constant. The underlying
  staleness is a pre-existing defect and stays out of scope.

## Design Details

### Commands

**Environment, pinned.** Three places, and every command below belongs to exactly one of them:

- **Local (darwin).** The whole Go module — including the CGO vendor detectors — builds and tests here,
  so nothing under `pkg/` needs a Linux host. The one thing that does **not** get checked locally is a
  `_linux.go` seam, which is not compiled on darwin at all.
- **The remote amd64 builder, over SSH.** The full image (a local build pulls ~20 GB of vendor base
  images and can fill the Docker VM's sparse backing volume) and the linux-only compile check for the
  seam. A remote `make` needs a login shell (`ssh <host> bash -lc '…'`) or the Go toolchain is off
  `PATH`.
- **The RDNA host, over SSH through the verification skill's own transport.** T1 and T8. Every command
  that touches a card is confirmed with the user before it runs (Boundaries, "Ask first").

```bash
# ---- Go verification, local (whole module builds and tests on darwin) ----
make lint          # whole-module golangci-lint + goimports; EDITS files; slow on a cold cache
make test          # go test -failfast -race -cover -timeout=30m; any args are EXCLUSION regexes
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -count=1 -cover \
    ./pkg/devicemanager/allocator/amd/... ./pkg/devicemanager/detector/amd/... \
    ./pkg/deviceplugin/... ./binding/hsa/...
# make generate is NOT needed: no API type changes (F4 reuses the existing placement transport).

# ---- the linux-only seam: it gets NO local check, so compile it where it is real ----
ssh <builder> bash -lc 'cd <checkout> && GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./...'

# ---- stage-level image build, local and emulated: the fast loop for T5 ----
# Check the Docker VM's backing volume first: the VM's own `df` misreports a sparse
# Docker.raw, and filling that volume corrupts the VM.
df -h /System/Volumes/Data

docker buildx build -f pack/gpustack-operator/Dockerfile --target xbuild-amd-rocm \
    --platform linux/amd64 --load -t amd-xbuild:amd64 .
docker run --rm --platform linux/amd64 amd-xbuild:amd64 bash -c \
    'ls -la /out; readelf -d /out/libvrocm.so | grep NEEDED; objdump -T /out/libvrocm.so | \
     grep -oE "GLIBC_[0-9.]+" | sort -uV | tail -1'
docker buildx build -f pack/gpustack-operator/Dockerfile --target xbuild-amd-rocm \
    --platform linux/arm64 --load -t amd-xbuild:arm64 .
docker run --rm --platform linux/arm64 amd-xbuild:arm64 bash -c 'ls -A /out | wc -l'   # expect 0

# ---- full image, on the remote amd64 builder: the F1 acceptance ----
ssh <builder> bash -lc 'cd <checkout> && make package gpustack-operator'
docker run --rm --platform linux/amd64 <image> bash -c '
    ls -la /etc/gpustack/lib/amd
    cat /etc/gpustack/lib/amd/ld.so.preload
    /etc/gpustack/lib/amd/rocm-monitor; echo "rc=$?"'
# rocm-monitor needs neither ROCm nor a device: what the run proves is that it REACHES its own
# "no usage region" message instead of dying in the loader. rocm-cumask-check cannot run here --
# it links the ROCm runtime by design -- so only its presence and NEEDED set are read.

# ---- T1, on the RDNA host: which device identity does ROCR_VISIBLE_DEVICES accept? ----
# Two cards, so the answer distinguishes "works" from "works by luck on card 0".
rocm-smi --showuniqueid; ls /dev/dri/renderD*
for v in 1 "GPU-$(rocm-smi --showuniqueid --csv | ...)"; do
    ROCR_VISIBLE_DEVICES="$v" python -c \
      'import torch;print(torch.cuda.device_count(), torch.cuda.get_device_properties(0))'
done
# then the same two forms with HSA_CU_MASK=0:0-11 and rocm-cumask-check, to prove the index
# space HSA_CU_MASK addresses is the post-ROCR_VISIBLE_DEVICES one on a NON-ZERO card.

# ---- the shim tree itself, unchanged by this spec but the thing being packaged ----
cd csrc/amd/rocm-slicing-shim && ./build.sh unit && ./vrocm_test    # no ROCm, no device
SKILL=.claude/skills/gpustack-operator-xbuild-and-verify
XB_MODE=ssh XB_HOST=<user>@<rdna-host> bash ${SKILL}/scripts/build.sh xbuild-amd-rocm
XB_MODE=ssh XB_HOST=<user>@<rdna-host> bash ${SKILL}/cases/amd-case-1.sh   # ... through amd-case-7.sh

# ---- T8, on the RDNA host: the injection as the allocator composes it ----
# Several packed windows, simultaneous and saturated; per container, in mode two:
rocm-cumask-check; echo "rc=$?"      # 0 = the mask took effect as asked
rocm-monitor                          # the quota this container was given, and what it has spent

# ---- chart, unchanged; run to prove it ----
make lint chart    # offline chart checks (NOT `make test chart`, which mutates a live cluster)
```

### Project Structure

```
pack/gpustack-operator/Dockerfile                       # + xbuild-amd-rocm-{amd64,arm64} stages and the
                                                        #   TARGETARCH alias, + COPY to
                                                        #   ${GPUSTACK_LIB_DIR}/amd, + install -D of the
                                                        #   preload asset beside the existing four
pack/gpustack-operator/external/amd/build-libvrocm.sh   # NEW: wrapper over csrc/.../build.sh lib|tool
                                                        #   + a call to its own `check`
pack/gpustack-operator/rootfs/etc/gpustack/lib/amd/ld.so.preload
                                                        # NEW: the library's container path, one line
api/worker/v1alpha1/devices.go                          # doc only: AllocatedPhysicalPlacements states
                                                        #   which unit applies in which allocation mode
binding/hsa/library.go                                  # + NumShaderEngines / NumShaderArraysPerSE /
binding/hsa/cgo_helpers_static.go                       #   NumXcc on AgentProperty and their getters
pkg/deviceplugin/types.go                               # + LogicalSlicedResponder (two methods, no
                                                        #   Rollback)
pkg/deviceplugin/server.go                              # + Sliced-mode occupancy read, the mutex-scoped
                                                        #   placement, retry reuse, and response-before-
                                                        #   patch (which also fixes a pre-existing strand)
pkg/deviceplugin/controller.go                          # + accumulateLogicalOccupied / LiveLogicalOccupied
                                                        #   / reservedLogicalOccupied, keyed by UUID
pkg/devicemanager/detector/amd/device.go                # + Status.LogicalSliced, + the missing
                                                        #   SetGroupSlicedDetails(grpList) call
pkg/devicemanager/allocator/amd/deviceplugin.go         # + Sliced server behind !opts.NoSliced,
                                                        #   + PlaceLogicalSliced / GetLogicalSlicedResponse
pkg/devicemanager/allocator/amd/cumask.go               # NEW: family selection, validation, the two
                                                        #   arithmetics and the packing -- pure, portable
pkg/devicemanager/allocator/amd/cumask_driver_linux.go  # NEW: the HSA topology read
pkg/devicemanager/allocator/amd/cumask_driver_other.go  # NEW: the stub
pkg/devicemanager/allocator/amd/cumask_test.go          # NEW: conformance tables A and B, degenerate
                                                        #   topologies, and the packing
pkg/devicemanager/allocator/amd/deviceplugin_test.go    # NEW: the sliced response cases
README.md                                               # matrix row
docs/architecture/discovery.md                          # mechanism table, env names, the fail-open note,
                                                        #   where the preload libraries come from
specs/2026-08-10-amd-gpu-sliced-injection.md
```

### Code Style

The derivation is the one place where a reader has to be able to check the code against the fixture line
by line, so it reads as the table does — the family first, the validation second, the alignment third,
and the reason each rule exists at its own site:

```go
// The branch is the ARCHITECTURE FAMILY, not the XCC count, and that distinction is the whole
// correctness of this function. gfx90a — MI210, and each GCD of an MI250X — is CDNA silicon that
// reports NUM_XCC = 1, so an "XCC > 1 means CDNA" test hands it to the RDNA branch and applies WGP
// pairing to a part that has no WGPs. csrc/amd/rocm-slicing-shim/build.sh compiles device code for
// gfx90a, so it is a part this product expects to meet.
func windowCUs(t Topology, pct int) (int, error) {
    // binding/hsa turns an unreadable agent-info field into a zero rather than an error, so
    // "absent" arrives here indistinguishable from "zero". Both must fail closed: a mask derived
    // from a zero confines nothing, and the platform reports neither.
    if err := t.validate(); err != nil {
        return 0, err
    }
    if t.CDNAFamily() {
        // Mask bits interleave across XCCs — bit i lands on XCC i mod X — so the atom is X CUs and
        // a bit count below one atom does NOT clamp down to a small slice: the XCCs it never
        // mentions run unmasked. Measured, `0:0` occupied 267 of 304 CUs while reading as a
        // plausible 3.7% of throughput. On a single-XCC part (gfx90a) X is 1 and this degenerates
        // to a contiguous window of any length — the least-assuming of the three rules.
        n := roundDiv(t.CU*pct, 100) / t.XCC * t.XCC
        if n == 0 {
            return 0, fmt.Errorf("%d%% of %d CUs is below one %d-CU atom on this card; its "+
                "smallest slice is %d%%", pct, t.CU, t.XCC, minPercent(t))
        }
        return n, nil
    }
    // RDNA allocates in WGP pairs, and the kernel spreads mask bits round-robin across shader
    // engines, so a WGP count that is not a multiple of S yields no throughput for the remainder.
    // The C reference implementation clamps a sub-round request UP to one round; this refuses
    // instead, deliberately: a 1% request would otherwise take a 10% ceiling while Kueue charges
    // 1%, and no conformance row covers below 10% either way.
    n := roundDiv(t.CU/2*pct, 100) / t.SE * t.SE
    if n == 0 {
        return 0, fmt.Errorf("%d%% of %d WGPs is below one %d-WGP round on this card; its "+
            "smallest slice is %d%%", pct, t.CU/2, t.SE, minPercent(t))
    }
    return 2 * min(n, t.CU/2), nil
}
```

The allocator's sliced branch follows the THead one key for key so the one difference — a mask where
THead has a percentage — reads as deliberate. Conventions this follows: snake_case multi-word Go
filenames; comments that state *why* a line is the way it is rather than what it does; errors wrapped
with the operation that failed; no helper extracted for a single call site; the `_linux.go` / `_other.go`
seam for anything that touches a binding. Dockerfile conventions follow
`pack/gpustack-operator/Dockerfile` — `ARG`s at the top, a heredoc `RUN` whose body is verbatim,
`set -exo pipefail`.

### Implementation Plan

Nine tasks. **Five are unblocked at the start with disjoint `Owns:`** (T1, T2, T4, T5, T6), so the DAG
fans out immediately. T1 is a hardware spike that settles a contract the allocator cannot be written
without; T2 is the arithmetic PoC whose fixture is already checked in. The checkpoint sits between the
code and the hardware verification, because only then is there something to verify.

- [ ] **T1 · Which device identity does `ROCR_VISIBLE_DEVICES` accept?**
      Blocked by: None
      Owns: `specs/2026-08-10-amd-gpu-sliced-injection.md` (F5's identity paragraph and F6 item 1)
      Gate: review
      Acceptance: on the two-card RDNA host, inside a ROCm container, both candidate forms are tried —
      a decimal index and a `GPU-<hex>` UUID — for a **non-zero** physical card, with and without the
      container-runtime hook's own restriction in play. The answer records: which form ROCr honours;
      whether `hipGetDeviceCount` and `torch.cuda.get_device_properties(0)` then name the intended card;
      and that `HSA_CU_MASK=0:…` addresses that same card, verified by `rocm-cumask-check` exiting `0`.
      A form that only works for card 0 is recorded as not working. If neither form composes with the
      hook, the fallback in Risks is recorded as the decision, with what it assumes about the node.
      Verify: captured output per form, folded into F5; every command confirmed with the user before it
      runs, per Boundaries

- [ ] **T2 · The mask derivation, against the conformance tables**
      Blocked by: None
      Owns: `pkg/devicemanager/allocator/amd/cumask.go`,
      `pkg/devicemanager/allocator/amd/cumask_test.go`
      Gate: review
      Acceptance: a `Topology{Name, CU, SE, SAPerSE, XCC}` with `validate()` and a family predicate
      driven by the gfx name; `windowCUs` reproducing conformance tables A and B; `packWindow`
      first-fit-then-least-overlap over a merged interval set, with start and length both multiples of
      `Q = 2S` (RDNA) or `Q = XCC` (CDNA); and a renderer producing `"<i>:<lo>-<hi>"`. Sub-quantum
      requests are refused on **both** branches with the card's minimum percentage in the message —
      diverging from the C reference's RDNA clamp deliberately, and changing no conformance row.
      `gfx90a` routes to the CDNA branch with `X=1`. No `binding/*` import: this file compiles and tests
      on darwin.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -count=1 -cover
      ./pkg/devicemanager/allocator/amd/...` — every row of both tables as a named case, the degenerate
      topologies (`CU=0`, odd `CU`, `SE=0`, `SE > CU/2`, `XCC=0`, `XCC > CU`, unknown gfx name) each
      refusing rather than panicking, and three mutations each failing exactly the rows that name them:
      drop the shader-engine alignment (table A's 25 % and 75 % rows), drop the XCC alignment (table B's
      5 % row), route by `NUM_XCC > 1` instead of by family (the `gfx90a` case)

- [ ] **T3 · Topology, read through HSA behind the platform seam**
      Blocked by: T2
      Owns: `binding/hsa/**`, `pkg/devicemanager/allocator/amd/cumask_driver_linux.go`,
      `pkg/devicemanager/allocator/amd/cumask_driver_other.go`
      Gate: (none)
      Acceptance: `AgentProperty` gains `NumShaderEngines`, `NumShaderArraysPerSE` and `NumXcc`, read in
      the existing `GetAgents` iteration through the constants already in `const.go`, in the same
      error-discarding style as their neighbours — with the zero-value hazard that creates handled by
      T2's `validate()`, not here. The linux seam maps an allocated card to its HSA agent by BDF, falling
      back to UUID exactly as `detector/amd/device.go:127-130` does, and returns T2's `Topology`; the
      other-platform stub returns a not-supported error. No `binding/*` import appears in an untagged
      file.
      Verify: `go build ./...` locally (the stub arm compiles, the seam is not built at all) **and** on
      the remote amd64 builder (the real arm) — a `_linux.go` seam gets no local check on darwin, so a
      linux compile is the only thing that proves it builds

- [ ] **T4 · The logical placement ledger in `pkg/deviceplugin`**
      Blocked by: None
      Owns: `pkg/deviceplugin/**`, `api/worker/v1alpha1/devices.go` (documentation comment only)
      Gate: review
      Acceptance: a `LogicalSlicedResponder` optional interface with `PlaceLogicalSliced` (called under
      the node mutex, its result published into `allocatedStatus` before `reserveDevices`) and
      `GetLogicalSlicedResponse` (called outside it, consuming — never recomputing — those windows);
      `Allocate` reads occupancy for the `Sliced` mode when the responder implements the interface, as it
      already does for `Partitioned`; the occupancy is keyed by accelerator UUID and carried on the
      existing `AllocatedPhysicalPlacements` field with an empty `AllocatedPhysicalProfile`, so the
      physical accumulator's profile gate (`controller.go:953`) skips it and the logical one gates on
      `Mode == Sliced` plus an empty profile; a retry reuses the container's own prior windows through
      the same lookup the partition path uses (`server.go:896-918`); and the container response is built
      **before** the durable annotation patch, with the reservation released on a build failure — which
      also closes the pre-existing strand at `server.go:1058-1062` for the NVIDIA and THead sliced paths.
      A responder that does not implement the interface is byte-identical to today, in both its response
      and its annotation.
      Verify: `go test -count=1 -cover ./pkg/deviceplugin/...` — `72.8 %` not regressed, the existing
      Allocate cases unchanged, and new cases mirroring the partition ones by name: two sequential sliced
      allocations on one card (the second sees the first's window); two concurrent ones (the second
      observes the first's window before the mutex is released); an allocation present in both occupancy
      sources counted once; a checkpoint-loss retry reusing the identical window; a responder failure
      releasing the reservation and leaving no annotation; and a sliced allocation's intervals invisible
      to both `accumulatePhysicalOccupied` and `priorPartitionTokens`

- [x] **T5 · The image builds and stages the shim**
      Blocked by: None
      Owns: `pack/gpustack-operator/Dockerfile`, `pack/gpustack-operator/external/amd/**`,
      `pack/gpustack-operator/rootfs/etc/gpustack/lib/amd/**`
      Gate: review
      Acceptance: F1 in full — `build-libvrocm.sh` delegating every compile decision to the tree's own
      `build.sh` and calling its `check`; `xbuild-amd-rocm-amd64` on `rocm/dev-ubuntu-22.04:7.2.4`;
      `xbuild-amd-rocm-arm64` a `${UBUNTU_IMAGE}` stand-in with an empty `/out`; the `TARGETARCH` alias;
      the final stage's `COPY` and `install -D`; and the one-line preload asset naming
      `/usr/local/vrocm/libvrocm.so`, which is a contract with T7's mount constant.
      Verify: the two stage-level `docker buildx build --target xbuild-amd-rocm` runs under Commands
      (amd64 lists three artifacts with `libvrocm.so`'s `NEEDED` at `libc.so.6` and its floor at
      `GLIBC_2.4`; arm64 lists none), then the remote full-image build and its `ls` / `cat` /
      `rocm-monitor` run

- [x] **T6 · The detector advertises the capability**
      Blocked by: None
      Owns: `pkg/devicemanager/detector/amd/device.go`
      Gate: (none)
      Acceptance: the per-card `status` block gains
      `device.AcceleratorLogicalSliced{Count: 128, CoresPercentageOvercommit: true}` and nothing else —
      the NVIDIA and THead shape, with no gate over the staged library, no architecture check and no
      extracted function — and `device.SetGroupSlicedDetails(grpList)` is called before
      `DetectAccelerator` returns, which AMD alone has never done. No new test: the addition is a struct
      literal inside a function that needs a real card, and its aggregate is covered in `pkg/device`.
      Verify: `make lint` clean and `go test -count=1 -cover ./pkg/devicemanager/detector/...` still
      passing, with the AMD package's coverage reported rather than hidden

- [ ] **T7 · The allocator hands out the slice**
      Blocked by: T1, T2, T3, T4
      Owns: `pkg/devicemanager/allocator/amd/deviceplugin.go`,
      `pkg/devicemanager/allocator/amd/deviceplugin_test.go`
      Gate: review
      Acceptance: `New` registers `newServer(logger, DeviceAllocationModeSliced)` behind `!opts.NoSliced`
      between the shared and visibility servers; `GetContainerAllocateResponse` stops discarding its Pod
      and container parameters and collects the `(group, accelerator)` pairs, its non-sliced response
      unchanged; `PlaceLogicalSliced` derives the window through T2 on T3's topology and packs it against
      the occupancy T4 supplies; `GetLogicalSlicedResponse` emits F5's six environment keys and five
      mounts, with the container paths matching T5's asset byte for byte and the identity form T1
      settled. A fixture modelled on `allocator/nvidia/deviceplugin_test.go:29`'s
      `redirectLogicalSliceDirs` redirects `OperatorLibDir` / `OperatorPodsDir` into `t.TempDir()`.
      Verify: `go test -count=1 -cover ./pkg/devicemanager/allocator/amd/...` — `0.0 %` → **≥ 85 %** —
      and three mutations each failing exactly the rows that name them: the mask's `GPU_list` index
      un-tied from the position in `ROCR_VISIBLE_DEVICES`, the MiB figure carrying HAMi-core's `"m"`
      suffix, and the ledger path moved to a node-wide location

- [ ] **Checkpoint.** `make lint && make test` clean; the remote amd64 image carrying all four files in
      `${GPUSTACK_LIB_DIR}/amd/` and the arm64 leg carrying only `ld.so.preload`; the seam compiling on
      linux. Only then is the capability real end to end, and only then is there something for T8 to
      point at hardware.

- [ ] **T8 · Verification on the RDNA host**
      Blocked by: T5, T7
      Owns: `specs/2026-08-10-amd-gpu-sliced-injection.md` (F6's result table)
      Gate: review
      Acceptance: F6 in full — several packer-generated windows of different sizes and offsets
      (25/25, 50/50, 25/50/25, and one oversubscribed set) running **simultaneously and saturated**, each
      container's `rocm-cumask-check` exiting `0` and its `rocm-monitor` reporting the injected quota,
      with a solo run per size supplying the ceiling the concurrent figures are read against. The
      aggregate throughput is what decides physical disjointness; cardinality alone is recorded as
      insufficient and why. The index alignment is asserted on a non-zero physical card. A run that took
      the by-hand fallback says so in its summary.
      Verify: recorded PASS/FAIL rows with captured output, folded into F6; every command confirmed with
      the user before it runs

- [ ] **T9 · Documentation**
      Blocked by: T5, T6, T7, T8
      Owns: `README.md`, `docs/architecture/discovery.md`
      Gate: (none)
      Acceptance: F7 in full — the README matrix row and its compute-ceiling note; `discovery.md`'s
      mechanism table, the `libvrocm.so` paragraph and what is different about a mask the operator
      derives, `LIBVROCM_LOG_LEVEL=1` with the per-denial reasoning, the preload-library provenance
      paragraph, and the fail-open subsection naming `rocm-cumask-check` and the per-card minimum
      percentage. Routed through the `gpustack-operator-docs` skill.
      Verify: the skill's index / link / table-of-contents checks pass, `docs/architecture.md` gains
      nothing, and `make lint` still passes

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- **There are no AMD tests to update — there are none at all.** `pkg/devicemanager/allocator/amd` and
  `pkg/devicemanager/detector/amd` each hold a single source file and no `_test.go`, measured at `0.0 %`
  today. So the prerequisite is not a rewrite but a first fixture: a sliced-path helper in the new
  `allocator/amd/deviceplugin_test.go`, modelled on `allocator/nvidia/deviceplugin_test.go:29`'s
  `redirectLogicalSliceDirs`, redirecting `deviceplugin.OperatorLibDir` / `OperatorPodsDir` into
  `t.TempDir()` — paths only, no files, because the response composes paths rather than reading them.
  Unlike the NVIDIA and THead fixtures it needs **no** faked character-device nodes: the AMD response
  carries no device nodes, the container-runtime hook injects them.
- **`pkg/deviceplugin`'s existing Allocate suite is the regression contract for T4** and must pass
  unchanged. The new optional interface must leave a non-implementing responder byte-identical, and the
  reordering of response-build and annotation-patch must not disturb
  `TestResourceServer_Allocate_RollsBackReservationOnPatchFailure` (`server_test.go:1265`) or
  `..._RecordsReservation` (`:1422`).

#### Unit tests

- `pkg/devicemanager/allocator/amd`: `2026-08-10` - `0.0%` → target `≥85%` (T2's two conformance tables,
  the degenerate topologies and the packing; T7's response cases)
- `pkg/deviceplugin`: `2026-08-10` - `72.8%` → not regressed, and expected to rise slightly with T4's
  logical-placement cases
- `pkg/devicemanager/detector/amd`: `2026-08-10` - `0.0%` (T6 adds no test: its statements sit inside
  `DetectAccelerator`, which needs a real card, and extracting them to buy coverage would be a test that
  restates a struct literal — the same call the THead arm made)
- `binding/hsa`: `2026-08-10` - `0.0%` (the three getters are cgo pass-throughs with no branch of their
  own; what proves them is T3's linux compile and T8's run on a real card)
- `pkg/nodefeature`, `pkg/device`, `pkg/worker/controllers/worker`: unchanged, and that is the claim —
  every capability, credit and capacity path this feature rides is vendor-blind and already covered. A
  diff touching them means the design drifted.

Cases worth naming because they encode a measured failure rather than an obvious branch:

- Conformance table A's 25 % and 75 % rows, whose naive derivation (`0:0-14`, `0:0-44`) measured **100 %
  of the card** on hardware.
- Conformance table B's 1 % row, refused; and its 5 % row, where 15 CUs of budget buy 8 CUs of card.
- A `gfx90a` topology (CDNA, `NUM_XCC = 1`) routing to the CDNA branch — the case that fails under an
  `NUM_XCC > 1` discriminator and passes under a family one.
- Every degenerate topology refusing rather than dividing: `CU=0`, odd `CU`, `SE=0`, `SE > CU/2`,
  `XCC=0`, `XCC > CU`, `CU` not divisible by the quantum, an unrecognised gfx name.
- Packing: first-fit over a hole; the same allocation present in both occupancy sources counted once;
  an oversubscribed card choosing deterministically; every window quantised on both axes.
- Two concurrent `ResourceServer.Allocate` calls proving the second sees the first's window in the
  reservation before the mutex is released.
- A responder failure after placement but before the patch, releasing the reservation and leaving no
  annotation.

#### Integration tests

The integration surface is the **image** and the **ledger**, and neither needs a cluster:

- **Stage level, both platforms** (local, emulated): `--target xbuild-amd-rocm --platform linux/amd64`
  yields `libvrocm.so`, `rocm-monitor` and `rocm-cumask-check`, with the library's four linkage
  assertions run *inside* the build so a regression fails the build rather than a later read;
  `--platform linux/arm64` yields an empty `/out`.
- **Image level** (remote amd64 builder): `${GPUSTACK_LIB_DIR}/amd/` carries those three plus
  `ld.so.preload`, whose single line matches T7's mount constant; `rocm-monitor` runs to its own "no
  usage region" message rather than dying in the loader.
- **Ledger, across a simulated restart**: the in-process reservations dropped and only the Pod
  annotations kept, a second allocation still sees the first's window. And the inverse: a Pod whose
  annotation is missing or malformed is skipped, which is recorded as the cross-mode ownership hazard it
  is rather than as graceful oversubscription.
- **Ledger, across a re-detect**: delete and recreate the `Devices` object with unchanged grouping, then
  with a changed group name/memory but the same accelerator UUID — the window survives both, because the
  occupancy is keyed by UUID.
- **Ledger, across Pod lifetime edges**: a Pod stuck `Terminating` keeps its window; a force-deleted Pod
  releases it. Both are assertions about the annotation lifetime this feature inherits, recorded so a
  change to that lifetime fails here rather than in production.
- `make lint chart` — the chart is untouched; this is the evidence, not a change.
- **No fake-client cluster test.** Every path this feature adds needs a node whose detector saw a card;
  a fake-client test would re-assert the unit tests through more machinery.

#### e2e tests

**F6's RDNA run is this feature's e2e**, not a supplement to one. `gpustack-operator-e2e` needs a cluster
carrying the accelerator it exercises, and on a cluster with no AMD card it covers none of this — not the
device-plugin registration, not the pool capability, not the injection.

What that run covers and nothing else does: the mount set as the kubelet actually applies it; the three
environment variables agreeing about which physical card they address, on a **non-zero** card; and
whether packed windows at different offsets are physically disjoint rather than merely differently
spelled. The last is why the run is several simultaneous saturated tenants rather than one container and
a probe: `rocm-cumask-check` compares occupancy cardinality on RDNA, so it can pass on two masks that
alias.

**What no test in this spec reaches**, stated rather than left to be discovered: the CDNA branch through
this feature's own Go. The available CDNA host is a rented instance that is itself a container, and
`gfx90a` — the single-XCC CDNA part the family routing exists for — was not available at all. The
branch's arithmetic is covered by table B's unit tests and re-checkable on any CDNA node by the mounted
probe; what is missing is a run of the allocator-composed injection there, and it is carried as an Open
Question rather than as an omission.

## Alternatives

- **Emit a fixed offset-0 mask and skip the ledger entirely.** Much smaller: no shared-machinery change,
  no packing. Rejected on measurement rather than on taste — two 50 % tenants then share one half of the
  card at 25.8 % each while the other half sits idle, which is worse for both of them than no mask at
  all. A compute quota that costs the tenant more than it costs the neighbour is not a quota worth
  shipping.
- **Choose the window in the vendor's `GetContainerAllocateResponse`.** The obvious place, and it cannot
  be made correct: that call happens after the node mutex is released *and* after the durable annotation
  is written (`pkg/deviceplugin/server.go:1036-1062`), so two concurrent allocations would first-fit onto
  the same window and neither choice would survive a restart. The placement is a mutex-scoped step of the
  generic server for the same reason the partition selection is.
- **A new API field for the CU windows** (`AllocatedLogicalPlacements`, protobuf 10). The earlier draft's
  choice, on the grounds that a field documented in memory-slice units should not carry CU bits.
  Rejected once the mechanism was checked: the physical accumulator already gates on a non-empty profile
  (`controller.go:953`), so a logical entry cannot pollute MIG accounting, and the only unguarded reader
  is reachable only from the partition branch. Reuse costs a documentation sentence and a test; a new
  field costs a CRD field, a protobuf number, a `make generate` cycle and a compatibility surface,
  permanently. The cheaper thing to own won.
- **Reconstruct every live container's window from its requested percentage instead of persisting it.**
  Looks stateless and is not: a live container keeps the mask it was injected with, so one Pod exiting
  shifts every reconstruction while the actual windows stay put, and the allocator then believes windows
  are disjoint that overlap.
- **Key the logical occupancy by `(Group, Device)`, as the physical ledger does.** Rejected because AMD's
  group ID is derived from the detected name and memory, so a re-detect that regroups a card orphans its
  ledger entry and the same physical card hands out its first window twice.
- **Discriminate the derivation on `NUM_XCC > 1`.** The earlier draft's choice, and wrong: `gfx90a` is
  CDNA silicon reporting one XCC, so it would be given RDNA's WGP pairing. Caught by design review before
  any code existed, which is what the review was for.
- **Clamp a sub-quantum RDNA request up to one shader-engine round**, as the C reference implementation
  does. Rejected on accounting: a 1 % request would take a 10 % compute ceiling while Kueue charges 1 %,
  and table A's lack of a reject row is not evidence of intent — it never tested below 10 %. Refusing on
  both architectures makes the behaviour one rule instead of two, at the cost of a Pod that was admitted
  and cannot start, whose message names the number that would work.
- **Read topology in the detector and persist it in the `Devices` API** instead of reading it in the
  allocator behind a seam. Debuggable and cgo-free at the allocation site, but it puts three more fields
  in the CRD to serve one caller in the same process, and the seam idiom for exactly this already exists
  in two allocators.
- **Gate the detector on `rocm-cumask-check`.** The safe-looking reading, and the shim spec proposed it.
  It cannot be built: the probe links the ROCm runtime, which the device-manager container does not
  carry. Recorded as a Non-Goal with that reason rather than as a decision.
- **Enforce compute in the library** with a token bucket or PID loop, as Ascend and THead do. Rejected by
  the shim spec on measurement: the platform already provides a hard ceiling, fair sharing and
  oversubscription at zero measured overhead.
- **A per-ROCm-major stage fan-out**, mirroring NVIDIA's. Measured unnecessary: the product links no ROCm
  object and one build was observed interposing across two ROCm majors.
- **Land the Go work now and the `pack/` wiring later.** Rejected for the reason the shim spec gave when
  it handed both off together: a detector that advertises a library the image never built places Pods
  that cannot start.
- **Stop at unit tests and image artifacts, as the THead spec did.** Reasonable there, because that
  vendor's injection carried no unmeasured contract. Here three items in the contract have never been
  measured through the allocator, and two of them fail silently when wrong.

## Open Questions

- **Is the CDNA arithmetic right on a single-XCC part?** `gfx90a` routes to the CDNA branch with `X = 1`,
  which makes its atom one CU and asserts neither fail-open rule — the conservative choice, and
  unmeasured. The named experiment is one `rocm-cumask-check` run per conformance percentage on an MI210
  or an MI250X GCD, which would also settle whether the shader-engine round-robin costs a remainder
  there. Needed before an MI200-series part is claimed as verified rather than supported.
- **Must an RDNA window's start be shader-engine-aligned, or only its length?** F4 assumes both, which
  costs nothing but placement granularity; the two measured offset rows (`0:2-15`, `0:16-29`) are
  consistent with either reading. T8 decides, and relaxing it is one constant.
- **Should the minimum requestable percentage be visible to admission?** Today a sub-quantum request is
  admitted and refused at `Allocate`, which leaves a Pod that cannot start. Publishing a per-card minimum
  in `AcceleratorLogicalSliced` and validating against it in the Pod webhook is the fix; it is API
  surface plus a validation rule plus a capacity-key question, and the floor differs per card (10 % on a
  60 CU / 3 SE part, 2.63 % on a 304 CU / 8 XCC one).
- **Should a rounded-down request report its effective percentage?** A 47 % request on a 3-SE RDNA card
  is served as 40 %, silently, while Kueue charges 47 %. The refusal path names its number; the rounding
  path does not, and no vendor in this repo publishes an effective figure today.
- **Do the CDNA rules hold under CPX/DPX partitioning, and across multiple cards?** Inherited from the
  shim spec unchanged. A CPX-partitioned agent reports `NUM_XCC = 1` because the card was *split*, not
  because the silicon has one XCC — the family routing sends it to the CDNA branch either way, which is
  the safe direction, but the atom size and the interleave have not been confirmed there.
- **Is 128 the right per-card slice count for AMD?** It is loose by design and the disjoint window count
  is much smaller (10 on the RDNA card measured, 38 on the CDNA one). If a per-card process or context
  ceiling turns up, the figure should follow it — but it should not be set to the window count, which
  would turn a packing property into a capacity ceiling.
- **When does the annotation-loss hazard get fixed?** An unreadable allocation record reads as a free
  card, which after a restart lets an opposite-mode allocation land on a card a sliced container still
  holds. It is pre-existing and node-wide, not AMD's, and it deserves its own effort — a fail-closed node
  condition rather than a silent skip.
- **Should the ledger region move somewhere a host-side scraper can read it?** It is under the pod work
  dir today, which a host process *can* read; whether that is the interface a future metrics path wants
  is a decision for whoever builds it. Same question THead left open.
