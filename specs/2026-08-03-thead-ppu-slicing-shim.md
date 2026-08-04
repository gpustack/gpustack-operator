# Spec: THead PPU slicing shim — the library logical slicing needs, and the evidence it works

Status: Shipped
Type: Feature

> **This spec ships the library, not the capability.** `libvppu`'s sources, the feasibility gates that
> prove the mechanism on real hardware, and the verification surface that keeps proving it. Turning it
> into something a user can request — the detector's `Status.LogicalSliced`, the allocator's `Sliced`
> server and its injection, and the `pack/` stage that builds and stages the library — is handed off,
> deliberately and for stated reasons; see Non-Goals. Until that lands, a slice happens only where
> somebody sets `LD_PRELOAD` by hand, which is how every case here runs it.
>
> Renamed from `thead-ppu-logical-slicing` after shipping, so that name stays free for the spec that
> delivers the capability. The `Task N of thead-ppu-logical-slicing` trailers in this branch's git
> history predate the rename; the commits were regrouped by module and the trailers dropped with it.

## Summary

GPUStack Operator already discovers and allocates THead PPU cards in exclusive and shared modes, but it offers
no **logical (software) slicing** for them: the THead detector never fills `Status.LogicalSliced`, the THead
allocator registers no `Sliced` server, and `pack/` contains no builder stage for a THead preload library. This
spec establishes that logical slicing for PPU is feasible, fixes the mechanism it must use, and builds
`libvppu.so`: both quota dimensions enforced — **per-card VRAM** and **compute** — over a cross-process ledger
that doubles as the usage surface a metrics scraper will later read.

It is worked in two stages, and the first is complete. **Stage 1 (delivered)** proved the mechanism on real
hardware and landed the two files that carry it, the visibility and enforcement halves. **Stage 2 (delivered, T12–T18)** turned those two files into the library: per-card quota keying, the `common/` ledger and its usage
region, the full driver-layer symbol surface, the compute PID loop, and the reader tool. What stays handed off is
the operator-side Go and the `pack/` wiring — see Non-Goals.

The research behind it is recorded in `.claude/reports/thead-ppu-logical-slicing.md` (187 findings, 180 verified
by three independent adversarial verification passes, plus a cross-model Codex review; 0 refuted). Two findings
are load-bearing and were confirmed on a live 16-card `PPU-ZW810E` host:

- `ppu-smi` reaches HGML through `dlopen("libhgml.so")` plus `dlsym` on the **explicit handle**, so preloading a
  library that merely defines `hgmlDeviceGetMemoryInfo` does **not** change what `ppu-smi` reports. Interposing
  the **`dlsym` call itself** does.
- THead follows the **ROCm deployment model**: the PPU SDK is installed in the *workload* container and the host
  only passes the device nodes through. It is not NVIDIA's model, where the container runtime injects the driver
  and management libraries. Everything about version handling and glibc floors follows from this.

The first shippable piece was independent of the rest and landed on its own: a `thead-ppu-devel` base image under
`pack/`, so the future slicing build stage can compile against SDK headers without every build fetching a
1.56 GB presigned archive. It merged ahead of everything else here
([#73](https://github.com/gpustack/gpustack-operator/pull/73),
[#74](https://github.com/gpustack/gpustack-operator/pull/74)) and is published as
`gpustack/thead-ppu-devel:2.1.1`.

The research report itself is not committed — `.gitignore` excludes `.claude/*` — so this spec is the tracked
record of its conclusions.

## Motivation

### Goals

- **Confirm the interception mechanism with evidence, not inference.** Close the one gap the read-only forensics
  could not: prove on real hardware that a container-scoped preload with a `dlsym` hook makes `ppu-smi` report a
  VRAM quota instead of the physical 98304 MiB.
- **Fix the design decisions** so Stage 2 does not re-litigate them: interception layer,
  module split, compute-quota algorithm, and where each artifact lives.
- **Ship the `thead-ppu-devel` base image** (own PR) and make `hack/package.sh` plus
  `.github/workflows/pack.yml` able to build and publish it — **done**, `gpustack/thead-ppu-devel:2.1.1`.
- **Extend `gpustack-operator-xbuild-and-verify`** so a THead host that has neither docker nor a running
  buildkitd can still execute the numbered cases.
- **Enforce both quota dimensions, per card.** VRAM and compute are independent dimensions in the request API
  (`.sliced.memory-percentage` and `.sliced.cores-percentage`), so a library that honours only the first turns a
  25%-compute request into a whole card's compute with nothing failing. Both are enforced against a per-card
  figure, because the allocator hands out one figure per card and a container may hold several.
- **Make usage readable from outside the container.** The cross-process ledger is the only place a slice's usage
  exists, and the compute *limit* exists nowhere else at all — no `ppu-smi` field carries it. Its layout is
  therefore a versioned contract rather than an implementation detail, so a metrics scraper can be added later
  without touching the shim.
- Success is testable: (1) the image builds and publishes for `linux/amd64` and a container from it compiles a
  shim with `NEEDED` limited to `libc.so.6` — **met**, `gpustack/thead-ppu-devel:2.1.1`; (2) each of the three PoC
  gates produces a recorded PASS/FAIL with captured output — **met**, all three PASS on a 16-card PPU-ZW810E
  host, recorded with their captured output under F2; (3) every interception symbol the
  module table names is re-established against that image's own libraries and checked in as a regenerable
  manifest, rather than asserted as a count nobody can re-verify — **met**,
  `references/thead-hggc-symbol-manifest.md`, regenerable against digest `sha256:5f83fd14…`;
  (4) one container holding two cards with **different** quotas is held to each card's own figure — **met**,
  `thead-case-6.sh` Part C, added at ship time after the spec review found the criterion carried no case at all:
  case 5 is two containers with one card each, which the spec itself calls a different claim, so **per-card
  keying had no behavioural test** and a shim charging one container-wide figure would have passed every row in
  the suite. Held **on both dimensions**: memory by that Part C, and compute by `thead-case-7.sh`'s two-card arm
  once `HGGC_DEVICE_SM_LIMIT_<i>` was added (F5) — one container, two cards at 50% and 25%, each load settling in
  its own band and both figures read back from the region at their own card's offset. The compute half was a real
  gap and not a wording one: until then the cap was a single container-wide figure copied into every card's slot,
  so "two cards at different compute caps" could not be expressed at all; (5) a compute quota below 100% measurably reduces the container's own utilisation while a second
  container on the same card keeps its share, read from `hgmlDeviceGetProcessUtilization`, which Gate 3 proved is
  supported and reports the container's own pid.

### Non-Goals

- **A `pack/` builder stage and a shipping build for `libvppu.so`.** Stage 2 finishes the *source*; wiring it into
  an image is the handoff. Nothing under `csrc/` is referenced by the `Makefile` or `hack/` today, and the repo's
  convention is a thin shell wrapper (`pack/gpustack-operator/external/nvidia/build-libvgpu.sh` delegates to
  HAMi-core's own CMake `build.sh`). `external/thead/build-libvppu.sh`, the `xbuild-thead-ppu` Dockerfile stage and
  the `ld.so.preload` install go together with the operator-side changes below, because a library the image does
  not build is a capability the node cannot advertise.
  - **What this does NOT exclude, as built (T16): the tree compiling itself for verification.**
    `csrc/thead/ppu-slicing-shim/build.sh` holds the translation-unit lists and flags, and the skill's
    `scripts/build.sh xbuild-thead-ppu` calls it inside the SDK image. Both backends that ship already have that
    separation; the alternative — recipes living in the verification cases — is what this spec had, and three
    cases had already drifted apart on one of them. The handed-off wrapper becomes a caller of this script rather
    than a second copy of it.
- **The operator-side Go changes.** Covered below; they gate on the image having built the library.
- **Anything requiring a second SDK generation.** See HGGC RT v2 below.

Still inside this spec, for the avoidance of doubt: `common/`'s cross-process ledger and locking, the full
`hggc/` symbol surface, compute throttling and its PID loop, `tools/`, and the fail-closed startup and per-card
quotas the two existing files lack. The gate-only artifacts stay separate under `.../testing/` so the boundary
between shipped code and test scaffolding is a directory rather than a comment.
- **The operator-side Go changes.** The THead detector's `Status.LogicalSliced` and the allocator's `Sliced`
  server go with `libvppu.so`, not here — the Ascend precedent gates the advertised capability on the operator
  image having actually built the library (`detector/ascend/device.go:497-524`), so advertising it before the
  library exists would be a claim the node cannot honour. Same for the `pack/` wiring that builds and stages the
  library, and for the `scripts/build.sh` arm that drives that build. The Project Structure section lists the
  boundary file by file.
- **HGGC RT v2 support.** No v2 artifact exists to compile or test against, so v2 work would be unverifiable.
  The design keeps the door open (driver-layer C signatures) but ships one product.
- **Physical slicing / MIG / SR-IOV vGPU.** `hgml.h` exposes 43 MIG and 79 vGPU functions and `binding/hgml`
  already binds the MIG surface, but `.partitioned*` is a separate family from `.sliced*` and a separate effort.
- **Turning software slicing into a security boundary.** It is cooperative isolation: static linking, musl,
  a direct `ioctl`, or removing the preload all bypass it. Same posture as the NVIDIA and Ascend backends.
- **Starting buildkitd on any user-owned host.**
- Changing how the operator discovers or allocates PPU cards in exclusive/shared mode.

## Proposal

Logical slicing for THead PPU is delivered as a preload library injected only into sliced *workload*
containers, enforcing a per-container VRAM quota and a compute quota, and made visible to `ppu-smi`. The
operator side reuses the existing per-vendor plumbing — a builder stage whose product lands under
`${GPUSTACK_LIB_DIR}/thead/`, the device-manager init container that stages that tree onto the host, and an
allocator branch that mounts the library plus a container-scoped `/etc/ld.so.preload`.

Two things are settled ahead of implementation because the research changed the obvious answer:

- The interception point is the **driver layer** (`libhggc.so`), not the runtime layer. The workload brings its
  own SDK, and the runtime layer's `hggcDeviceProp` struct layout changed between RT generations while the
  driver layer's memory and launch signatures are simple and stable.
- Visibility requires interposing **`dlsym`**, because `ppu-smi` resolves HGML entry points through an explicit
  `dlopen` handle. Defining the HGML symbols alone is inert.

### User Stories

#### Story 1
As a GPUStack Operator maintainer, I want a published `thead-ppu-devel` base image that carries the PPU SDK, so
that the logical-slicing builder stage compiles against SDK headers without every build fetching a 1.56 GB
presigned archive.

#### Story 2
As a GPUStack Operator maintainer, I want the interception mechanism and module split confirmed against real
hardware before implementation starts, so that the implementation builds against a verified mechanism
instead of an inference.

#### Story 3
As a platform user running workloads on a THead PPU node, I want a sliced container to see only its own VRAM
quota in `ppu-smi` and to be denied allocations beyond it, so that several workloads can share one PPU card with
predictable capacity.

#### Story 4
As an operator developer verifying a build change, I want `gpustack-operator-xbuild-and-verify` to run its cases
on a THead host that has no docker, so that I do not have to install docker or start buildkitd on someone
else's production node.

### Core Features & Acceptance Criteria

#### F1 — `pack/thead-ppu-devel` base image (own PR, lands first) — **delivered**

A build-time base image containing the PPU SDK, published through the existing auxiliary-image channel.

Shipped in [#73](https://github.com/gpustack/gpustack-operator/pull/73) (the image) and
[#74](https://github.com/gpustack/gpustack-operator/pull/74) (two build-breaking defects in it). The published
artifact is **`gpustack/thead-ppu-devel:2.1.1`** — the tag tracks the SDK package version, not the operator's.
Everything below is the delivered behaviour; `pack/thead-ppu-devel/Dockerfile` is the authority if they diverge.

- `pack/thead-ppu-devel/Dockerfile` exists and installs the SDK to `/usr/local/PPU_SDK`, matching the path the
  operator image's `ENV PPU_HOME` already uses.
- The SDK is obtained via `--noexec --keep --target` self-extraction, **not** by running `install-ppu.pl`: on a
  clean `ubuntu:18.04` the installer exits 2 with `Can't locate Tie/File.pm in @INC`, because the base image
  ships only `perl-base`. Extraction avoids the dependency entirely.
- A build-time version gate asserts `hggcrt_version:<expected>` in `PPU_SDK/VERSION.txt` and fails the build
  otherwise. Note the older `ppu-sdk-1.0` layout has no `VERSION.txt`, so the gate must fail loudly rather than
  skip when the file is absent.
- `ARG BASE_IMAGE` defaults to `ubuntu:20.04` — deliberately older than the operator runtime image (24.04) and
  matching the SDK package's own target (`ubuntu2004`, glibc 2.31), lowerable to `ubuntu:18.04` (glibc 2.27,
  verified working) when an older workload base must be supported. The binding guard is **not** the base tag but
  the build-time assertion: a shim compiled inside the image reports no `GLIBC_` requirement above the SDK's own
  floor (`GLIBC_2.17`).
- The same build-time smoke test asserts two more properties, both of which broke a real build: `hgml.h` carries
  **zero `#include` lines**, so a consumer must supply `<stdbool.h>` and `<stddef.h>` itself (or `gcc -include`)
  before including it — it provides neither the `bool` its own declarations use nor `NULL`; and `DT_NEEDED` must
  never name an HGGC/HGML library, since the product is preloaded into a container that brings its own SDK. `libc.so.6`
  or nothing at all are the only correct entries — a stub this small references no libc symbol, and Ubuntu's
  default `--as-needed` then records none.
- `ARG PPU_SDK_URL` is documented in a Dockerfile header comment as a presigned, ~7-day-expiry URL that must not
  be committed or logged.
- The image is **`linux/amd64` only**. The SDK ships `targets/x86_64-linux` and nothing else, and the repo's own
  `pack/gpustack-operator/rootfs/lib/aarch64-linux-gnu/gpustack/` holds only a `.gitkeep` while the x86_64
  sibling carries `libhgml.so` and `libuki.so`.
- `hack/package.sh` passes `PPU_SDK_URL` through as a `--build-arg` when the variable is set, so
  `PPU_SDK_URL=… make package thead-ppu-devel` works. The task loop itself needs no change: `pack "$@"` is
  generic and skips any task without a `Dockerfile`.
- `.github/workflows/pack.yml` gains `thead-ppu-devel` in the `repository` choice list, a free-form build-arg
  override input modelled on `gpustack/runner`'s `args` (space-separated `KEY=VALUE`, expanded into
  `build-args:`), and a **per-image matrix** so `thead-ppu-devel` builds amd64 only while `ssh-server` keeps
  amd64 + arm64. The `manifest` job derives its platform list from the matrix output, so it follows automatically.
- Acceptance (**met**): a `workflow_dispatch` run with `repository=thead-ppu-devel`, a tag, and
  `args=PPU_SDK_URL=<presigned>` publishes a single-platform image, and `docker run --rm <image> bash -c 'cat
  $PPU_HOME/VERSION.txt'` prints the expected `hggcrt_version`. `gpustack/thead-ppu-devel:2.1.1` is that image;
  F3's modules build against it. Note no *automatic* CI job builds this Dockerfile — `pack.yml` is
  `workflow_dispatch`-only and the presigned URL has to be supplied by hand, so a green `ci` run on a change to
  this file is not evidence that it still builds.

#### F2 — PoC gates (must all pass before implementation begins)

Three container-scoped experiments on a real PPU host. All are read-only with respect to the host: no write to
the host `/etc/ld.so.preload`, no host library replaced, no daemon started.

- **Gate 1 — `ppu-smi` interception.** With a container-scoped preload of a shim that hooks `dlsym` and rewrites
  `hgmlDeviceGetMemoryInfo` and `_v2`, `ppu-smi` inside that container reports the configured quota instead of
  98304 MiB. A control arm that defines the HGML symbols but does **not** hook `dlsym` must show the physical
  value — that control is what proves the mechanism rather than a coincidence. A third arm runs with the
  vendor's `libhggc_wrapper.so` also preloaded and must not recurse or deadlock.
  **Met, and confirmed at two independent levels.** Without any hardware, the mechanism itself is decidable
  against the real `2.1.1` libraries: an inline program that `dlopen`s `libhgml.so` the way `ppu-smi` does
  reports, via `dladdr`, that with the hook preloaded both `hgmlDeviceGetMemoryInfo` and `_v2` resolve
  **inside the shim**, and that with the control preloaded they still resolve **inside `libhgml.so`** while
  the control's own constructor marker proves it loaded. That row is why this spec's central claim never
  rested on inference alone. The end-to-end reading it cannot produce — `ppu-smi` stops at
  `init HGML error: driver is not loaded` before it ever resolves a memory getter — is the hardware half, and
  it is recorded below, measured on a card.
- **Gate 2 — memory-path completeness.** Quota enforcement holds across the plain path, the VMM path
  (`hgMemCreate`/`hgMemMap`), the pool path (`hgMemAllocFromPoolAsync` plus `hgMemPoolTrimTo`), and the async
  path (`hgMemAllocAsync`); each is confirmed to reach `libhggc.so`. The workload's entry point and the
  interposed symbol are deliberately different names: `hggcMalloc` lives in the **runtime** library
  `libhggcrt.13.0.so`, its driver-layer counterpart is `hgMemAlloc_v2`, and the gate is precisely the
  question of whether the first funnels into the second — so the shim interposes the driver name and the
  test calls the runtime one. If any branch reaches `libalippu.so` without passing through `libhggc.so`, the
  driver-layer-only premise is recorded as broken; a case where **no** counter moved at all is not that
  finding, it means the shim watched a name nobody called.
- **Gate 3 — compute throttling.** `hgmlDeviceGetProcessUtilization` returns data on real hardware, and the
  reported `pid` is characterised as host or container PID. This decides whether the PID loop's feedback input
  can be per-process.
- Acceptance (**met — all three gates PASS**): each gate produces a PASS/FAIL row with captured command output; a
  FAIL is a recorded finding, not a silent retry.

##### Gate results, measured on a 16-card `PPU-ZW810E` host

Run through `XB_MODE=ssh XB_CTR=nerdctl XB_CTR_ARGS='--namespace k8s.io'` against the published
`gpustack/thead-ppu-devel:2.1.1`, on cards the idle picker chose (card 0 was holding 90797 MiB of production
inference throughout and was never touched; it still held it afterwards). The three shims' `sha256` matched the
local build byte for byte, so what ran is what `thead-case-1.sh` asserted.

**Gate 1 — `ppu-smi` interception: PASS.** `cases/thead-case-2.sh`, quota 4096 MiB on card 1.

```
PASS | baseline physical figure | card 1 total=98304MiB (measured, not assumed)
PASS | arm a: hook reports the quota | total=4096MiB == quota, interception marker present
PASS | arm b: control reports the physical figure | total=98304MiB == baseline, and the control proved it loaded
PASS | arm c: vendor wrapper coexists | total=4096MiB == quota with libhggc_wrapper.so also preloaded
--- ppu-smi arm a (card 1) ---
[vppu] intercepted dlsym(hgmlDeviceGetMemoryInfo)
| N/A  35C   N/A       94W / 400W | 1MiB / 4096MiB       |   0%        Default  |
```

**Gate 2 — memory-path completeness: PASS**, and the driver-layer-only premise holds on all four paths.
`cases/thead-case-3.sh`, quota 4096 MiB, 1024 MiB under / 8192 MiB over. Every path's counter names the entry
that fired, which is what proves the crossing rather than inferring it — the plain path's `hgMemAlloc_v2=1` is
the runtime's `hggcMalloc` arriving in `libhggc.so`:

```
plain: DENIED hgMemAlloc_v2 request=8589934592 accounted=0 quota=4294967296
       counters: hgMemAlloc_v2=1 hgMemFree_v2=1
async: DENIED hgMemAllocAsync ...            counters: hgMemAllocAsync=1 hgMemFree_v2=1
pool:  DENIED hgMemAllocFromPoolAsync ...    counters: hgMemAllocFromPoolAsync=1 hgMemFreeAsync=1
vmm:   DENIED hgMemCreate ...                counters: hgMemCreate=1 hgMemMap=1 hgMemUnmap=1 hgMemRelease=1
```

**Gate 3 — compute feedback input: PASS, per-process feedback is available.** `cases/thead-case-4.sh`.
`hgmlDeviceGetProcessUtilization` is supported at runtime and returned non-empty samples under the probe's own
copy load, with `smUtil` rising as the load ran; the reported `pid` equalled the probe's own, so it is in the
**container's** PID namespace. The PID loop therefore needs no host-PID translation and no fallback to card-total,
which retires that risk:

```
PROBE util_call round=1 rc=0 count=1 msg=Success
PROBE sample round=1 pid=1 smUtil=6 memUtil=0
PROBE sample round=2 pid=1 smUtil=12 memUtil=0
PROBE proc pid=1 usedGpuMemory=67108864
PROBE VERDICT util=supported samples=3
PROBE VERDICT pidns=container
```

**Multi-card independence (case 5): PASS.** Cards 1 and 2, quotas 2048 and 6144 MiB, containers run
concurrently: the between-quota 4096 MiB was refused on A and served on B, and A's marker named A's own figure
(`quota=2147483648`), so no accounting leaked between them.

Three defects in the cases themselves surfaced only once real hardware ran them, each of which the no-hardware
`SKIP` path had hidden, and all three are fixed: `LD_PRELOAD` was given the **host** staging path instead of the
container mount point (the loader then ignored the shim and the arms read the physical figure); the SDK
**renumbers devices inside the container**, so a container given one card node addresses it as index `0` while the
host ordinal only names `/dev/alixpu_ppu<N>`; and this host's login banner was not in `XB_BANNER_RE`, so it became
the first line of `thead_idle_cards` and would have been consumed as a card index.

##### One product defect and three false-PASS paths the gates could not see

A cross-model review of the branch at ship time found a defect the whole PASSing run above was blind to, plus
three ways a row could report PASS without its property holding. All are fixed and the gates were re-run on the
same host afterwards, with the same result and card 0 untouched.

The product defect is in `hggc_quota.c`'s ledger. It deleted an entry by **emptying** its slot, which with open
addressing truncates the probe chain of every key that had probed past it — and because device pointers are
page-aligned, `key % 1024` was `0` for essentially every allocation, so the table was one degenerate chain out of
slot 0 and *any* free but the most recent one broke lookup for everything after it. The bytes then stayed charged
for the life of the process. Fixed with tombstone deletion plus a key mix, and reproduced against the pre-fix
source to prove the new row is a real discriminator rather than decoration — both frees returned `rc=0` and the
shim counted them (`hgMemFree_v2=2`), yet `accounted=2147483648` survived them and the next request died on our
own quota:

```
[vppu] DENIED hgMemAlloc_v2 request=4294967296 accounted=2147483648 quota=4294967296
PATH refund step=hggcFree.first rc=0
PATH refund step=hggcFree.second rc=0
PATH refund result=failed rc=2
```

Why four PASSing path groups could not see it: each observation ran **one** allocation in a fresh process, so the
refund path was never executed and the corrupt ledger died with the process. Case 3 now carries a fifth row that
fills the quota with two allocations, frees both, and asks for the whole quota again — admitted only if both
refunds landed. The other three fixes close the same class of hole elsewhere: Gate 1's arm (c) never proved
`libhggc_wrapper.so` had loaded, so an absent wrapper left the hook working alone and still reported the quota
(it now asks the loader through `LD_DEBUG=libs`, and an unloadable wrapper is a SKIP rather than a PASS); the
utilisation probe counted a sample from **any** pid as proof of per-process feedback, though the query asks for
all history, so a neighbour's stale sample would have done (it now needs a sample carrying the probe's own pid,
and reports `others-only` when it does not get one); and case 5 required two idle cards but not two *distinct*
ones. Two quota-arithmetic overflows were fixed alongside them — a request large enough to wrap
`accounted + bytes` was admitted with no `DENIED` marker, and a `mib` large enough to wrap `mib * 1MiB` turned a
configured quota into either 0 (enforcement off) or a figure small enough to deny everything.

#### F3 — Module design

The four library modules are built here, in Stage 2. The two operator rows stay handed off: they gate on the image
having built the library, which is the `pack/` work Non-Goals excludes.

| Module | Responsibility | Fixed constraints |
| --- | --- | --- |
| `common/` | Quota parsing, cross-process ledger, locking, logging, and the usage region | No `hg*`/`hggc*`/`hgml*` type may appear. Lock is held until the real allocation returns (closes check-then-alloc). Quota must be re-read rather than frozen by the first ledger creator. The region's layout is a versioned contract, because F5 makes it the usage surface as well as the ledger. |
| `hggc/` | Driver-layer quota enforcement | Covers the allocation, free, query, and pool symbols plus **all** suffix variants, and `hgGetProcAddress`, `hgGetProcAddress_v2`, `hgGetExportTable`. Launch throttling covers all launch entries, including `hgGraphLaunch(_ptsz)` and `hgLaunchCooperativeKernel(_ptsz)`. Measured in `gpustack/thead-ppu-devel:2.1.1` (`hggcrt_version:v3`): 620 exported `hg*` symbols, 183 suffixed, 437 base names, **16** launch entries — the research's 23 was the one count the image did not confirm. `hgMemAlloc`, `hgMemFree` and `hgMemGetInfo` each export a plain **and** a `_v2` symbol, and the plain forms are the v1 ABI with different parameter types, so covering them means writing the v1 prototypes rather than reusing the v2 ones. |
| `hgml/` | Slice visibility | Must hook `dlsym`; must cover both public memory getters separately (their shared helper is `FUNC LOCAL`, not interposable); must include a re-entrancy/origin guard against the vendor wrappers. **As built (T17): `used` comes from `common/`'s ledger, and the hook must be preloaded before any other `dlsym` interposer — two of them step over each other rather than chaining, so behind one this shim is inert (F5).** Optional UKI fallback (`ppuGetDeviceRuntimeInfo`) is a later phase because UKI ships no header. |
| `tools/` | Per-container quota/usage reader | The only place a compute *limit* can be displayed; `ppu-smi` has no equivalent field. Reads `common/`'s region rather than the shim's own state, so the same path serves a future scraper. **As built (T18): `ppu-monitor`, preloaded into nothing. It links none of `common/`'s ledger code — that code maps the region lazily, so it would *create* one, and its other entries take the card's lock; a reader must do neither — which is also why it needs neither the SDK nor a device. The layout is written down as a contract and checked against a region case 1 writes from that document alone.** |
| operator detector | Advertise `Status.LogicalSliced` | Gate on hardware/driver facts the device-manager can actually see, **not** on `VERSION.txt` — it holds no SDK and the workload does not exist yet at detect time. |
| operator allocator | Inject into sliced containers | Existing device nodes plus three mounts — container-scoped `/etc/ld.so.preload` (ro), `libvppu.so` (ro), lock/ledger dir (rw) — with `ppu-monitor` riding the library's own mount, the way the Ascend allocator mounts `enpu-monitor`; and F5's environment: a memory figure **and** a compute figure **per card**, the compute one **even when it is 100%**, and a quiet `LIBHGGC_LOG_LEVEL` that a workload's own value overrides. Emitting `HGGC_DEVICE_SM_LIMIT_<i>` rather than only the un-indexed figure is what makes a per-card compute cap requestable — the NVIDIA branch emits only the un-indexed `CUDA_DEVICE_SM_LIMIT`, and copying that would leave the library's per-card cap reachable by hand alone. No host library injection and no runtime-major directory selection are needed. |

- Compute quota uses **PID feedback**, not a token bucket, with three departures from the reference
  implementation: feedback input is this container's per-process utilisation rather than card-total (card-total
  couples every container's controller and oscillates); behaviour is fail-closed (a missing quota config in a
  sliced container is an init error, and an unresolvable device index is an error, never a fixed sleep); and the
  loop starts from a quota-implied delay so a cold-start burst cannot take the whole card before the first
  sample. Gains are env-tunable and are not inherited from the reference values.
- Graph accounting starts as a launch-time coefficient defaulting to **off**, with graph and non-graph
  utilisation logged separately; instantiate-time node accounting is an escalation taken only if measurement
  shows graphs escaping. Rationale: with a closed loop, mis-modelled graph weight is absorbed by the integral
  term, while node accounting would introduce the design's only per-object state table plus four staleness modes
  (`hgGraphExecUpdate`, `hgGraphNodeSetEnabled`, conditional nodes, nested child graphs).
- **As built (T14), the memory surface is 35 exported names**, and what each wrapper does matters more than the
  count: ten allocating entries are charged, five freeing ones refund, the two queries answer with the quota's
  figures, and host memory, address mapping and the eleven pool entries are counted and never charged — pinned host
  pages are not device VRAM, mapping only binds a handle `hgMemCreate` already paid for, and a pool's memory is
  taken by `hgMemAllocFromPoolAsync`, which is charged. They are interposed anyway, so a path crossing
  `libhggc.so` stays a counted fact rather than an assumption.
  - The pitched entries are the only allocations whose true size is not knowable before the call — the driver picks
    the row stride — so admission is decided on the caller's width and the charge reconciled to `stride × height`
    under the same lock. Freeing a successful allocation behind the caller's back to hold the figure exactly would
    break a working workload over padding it never asked for, so a stride that overruns the quota is reported
    instead of refused.
  - An imported pool pointer is deliberately **not** recorded: it maps an allocation another process already paid
    for, and recording it would let this container's free credit it for memory it never took.
  - The host-memory frees deliberately do not refund either. Nothing recorded a host pointer, so a lookup could
    only ever match a device handle carrying the same number — a refund for memory that was never freed.
- **`hgGetProcAddress` on `2.1.1` already hands back the interposed address**, measured rather than assumed: it
  resolves through the linker like any other caller, so a preloaded definition wins there too and an entry taken
  through it is charged with no substitution needed. The substitution is therefore a guard against a driver that
  returns its internal implementation — NVIDIA's counterpart does exactly that — not the mechanism that makes case
  3's resolver row pass today. Which of the two happened is printed rather than inferred: a substitution that
  silently matched nothing would look identical to one that worked.
  - `hgGetExportTable` is the one acknowledged gap. The table it hands back is opaque, and guessing at offsets to
    swap pointers inside it would corrupt the runtime rather than account for it, so the request is logged with the
    table's identifier and the per-entry counters are what keep the gap from being a silent one.
- **The `_ptsz` variants and the v1 names each needed a way to keep the header type-checking them**, which is the
  difference between a wrapper that reads its caller's arguments and one that reads them off by whole registers.
  The header maps a plain source name onto the `_ptsz` symbol only under `__HGGC_API_PER_THREAD_DEFAULT_STREAM`,
  which the product must not define — it would move every stream entry at once — so each variant is declared as
  `__typeof__` of the plain entry rather than retyped. The v1 forms are the opposite problem: reaching the plain
  exported name means `#undef`-ing the header's mapping, which leaves five hand-written signatures with nothing
  checking them, so case 1 adds a syntax-only compile under `__HGGC_API_VERSION_INTERNAL` +
  `__HGGC_API_VERSION_UMD`, where the header declares the v1 prototypes itself. That row is proven non-vacuous:
  retyping one size to `unsigned long` makes it fail with `conflicting types for 'hgMemAlloc'`.
- **As built (T15), the launch surface is 16 exported names and the cap they spend is a duty-cycle window.** Each
  card carries a repeating window; a launch inside its open part goes through and one after it waits for the next
  window, so a workload launching back to back gets the container's share of wall time. The window is chosen over
  HAMi-core's token bucket for one reason that matters more than taste: the cold-start allowance is then
  `window × limit%` and is **exact**, where a bucket needs a launches-per-period ceiling to derive the same floor
  from — a hardware figure nobody has measured, and one that would silently hand out the whole card on the first
  window if guessed too high. It also makes the fail-closed path natural: when utilisation cannot be read at all,
  holding the last allowance still enforces a quota-derived duty cycle rather than no cap.
  - The window lives in `common/`'s region, so every process in the container divides **one** window. Whichever
    notices it has run out restamps it, and there is no lock on the launch path: a record lock is a system call and
    a launch is not, so the shared words are read and published with atomics.
  - Host callbacks (`hgLaunchHostFunc{,_ptsz}`) are counted and never gated — they run on the CPU, so delaying one
    frees none of the card. `hgLaunchCooperativeKernelMultiDevice` is gated on the **calling** context's card:
    waiting on several windows at once would need a lock order this design has no reason to define, and the entry is
    counted so a workload using it is visible rather than assumed absent.
- **The loop's rate is set by its sensor, and the sensor was measured rather than assumed.** Traced on a PPU, this
  driver's per-process utilisation figure is slew-rate limited to about ten percentage points per hundred
  milliseconds in both directions: a card going from idle to pinned reads `0, 10, 22, 32 …` and needs a full second
  to say `100`. A loop stepping once per 100 ms window therefore acts on a figure up to a second stale in the
  direction it has just moved, and the first build that did exactly that oscillated between the whole window and the
  floor with a period of seconds. So the window and the step are **two timescales**: the window stays short so a
  throttled workload waits in small pieces, and the step defaults to the measured second
  (`HGGC_SM_CONTROL_STEP_MS`). This is the observation the reference implementations could not have supplied, since
  their gains were fitted to NVIDIA hardware.
- **The feedback is filtered to this container through the ledger's own process table.** The region file is per
  container, so the pids in it are this container's by construction, where a pid-namespace test could not answer the
  question — a host pid may well also be a valid pid here. This is load-bearing and was measured as such: with the
  filter removed each loop reads the card total, and two containers capped at 25% settle near **13% each** instead
  of 25%. That number is also what tightened case 7 — its two-container row originally accepted anything non-zero
  and passed the defect it exists to catch.
- **The decision half of the loop lives in `common/`, which is a deliberate departure from "the PID loop lives in
  `hggc/`".** Reading a card and gating a launch need a PPU; deciding what the next allowance should be needs
  nothing, and it is the half whose failure mode on hardware reads only as "it oscillates". Since `common/` may
  name no `hg*` type, the arithmetic there is exercised with no device at all: the cold-start floor, both clamps,
  integral windup, and convergence against a simulated card whose utilisation overruns the window it was given.
  Four mutations were run to keep those tests from being decoration — a hot gain, a removed integral clamp, a window
  allowed to close completely, and a region that publishes nothing — and each fails a named row.
- **The compute figure now decides whether the container's configuration is usable**, which is the flip this stage
  carried from T13. A card the container holds whose compute figure is absent or unparsable — `HGGC_DEVICE_SM_LIMIT_<i>`
  or the un-indexed figure it falls back to — refuses every allocation *and* every launch,
  because the allocator's own helper defaults a missing compute request to 100% and treating the variable as
  optional would hand out a whole card's compute in silence. The cost is visible in the cases: every one that
  injects the shim now injects the compute figure too, at `100` where it is meant to uncap.
- **The graph coefficient tightens the window rather than weighting a token.** A graph launch is admitted only in
  `allowance / weight` of the window, since it runs however many kernels were captured into it; at the default
  weight of `1` graph launches are gated exactly like any other. The separate logging the design asked for is
  honest about what the driver can supply: there is **one** utilisation figure per process, so graph and non-graph
  utilisation cannot be read apart directly. What is separated is the measurement — an interval in which a graph
  launch was issued accumulates apart from one in which none was, and the two averages are printed side by side, so
  a graph average sitting above the plain one is the measurement that would justify raising the weight.
- **As built (T17), the visibility half reports the ledger's figure, and stacking it against a second `dlsym`
  interposer measured something this table had backwards.** `total` was already the card's quota; `used` is now
  `common/`'s accounted total for that card, so `ppu-smi` and an admission decision work from one number. That was
  not a formality: the same slice holding 2048MiB reads **2081MiB** through the vendor's own figure, because the
  driver counts context and overhead the container's quota never charged for — two numbers that were quietly
  different. A card with no configured figure is still left transparent, which is deliberate symmetry with the
  enforcement half's own `hgMemGetInfo` view: both report the vendor's figures when there is no quota to report
  instead, so the driver-layer query and `ppu-smi` never disagree. Admission is the half that refuses.
  - **Two `dlsym`-interposing libraries do not chain through each other.** Interposing `dlsym` forces a versioned
    lookup — `dlvsym(RTLD_NEXT, "dlsym", "GLIBC_x.y")`, because calling `dlsym` by name inside a `dlsym` hook calls
    the hook — and a versioned lookup does not match an unversioned definition in an object that carries a version
    table, which every one of these does from its libc imports. So each one's `RTLD_NEXT` steps over the other and
    lands on libc. Whoever the loader reaches first owns the symbol; the other is loaded, initialised and never
    entered. Measured in both orders, hardware-free, in case 2.
  - That is a **stronger** answer than the re-entrancy guard this table asked for: a peer cannot recurse back in
    through this chain at all. The guard is kept and is honestly unexercised — removing it changes no row in case 2,
    which was checked rather than assumed. It stays because it is one thread-local test on a path that is not hot,
    and because the origin half covers what no ordering argument does: the wrapper pointers are per process while a
    guard is per thread, so a resolution on a second thread must not be able to store our own wrapper as the real
    function.
  - The consequence that is **not** free is an ordering constraint on F5's injection contract, recorded there: the
    visibility shim is inert behind another `dlsym` interposer, and nothing inside the library can change that.
- Acceptance (**met**): every symbol named in this table is re-established against the libraries inside
  `gpustack/thead-ppu-devel:2.1.1` — not the host's copies, which is where the original counts came from — and
  checked in as `references/thead-hggc-symbol-manifest.md` carrying the names, the image digest and the command
  that regenerates it. A count that cannot be reproduced is not evidence. Delivered against digest
  `sha256:5f83fd14…`; re-running the recorded command reproduces the manifest's generated block byte for byte.

#### F4 — `gpustack-operator-xbuild-and-verify` extension

- A THead backend: three hardware WARN rows in `scripts/preflight.sh` (`ppu-smi`, the `/dev/alixpu*` nodes, the
  `alixpu` module), a third Cases table and env-knob group in `SKILL.md`, and the `allowed-tools` entries for the
  new case scripts — that list is per-script with no glob, so an unlisted script cannot run.
- **`scripts/build.sh` gains an `xbuild-thead-ppu` arm (as built, T16), and the earlier decision not to was
  reversed.** The reasoning against it was that the arm *builds* `libvppu.so` and the build system is a Non-Goal.
  That conflated two things: the **product** build — the Dockerfile stage, the `pack/` wiring, the `ld.so.preload`
  install — which is still handed off, and a **verification** entry point that compiles the checked-in sources so
  the cases have something to inspect. The other two backends have exactly the latter, and without it the THead
  cases had to carry the compile recipes themselves, which is where they drifted (see T16). The arm is also not a
  Dockerfile build: with no `xbuild-thead-ppu` stage in `pack/` yet, and a PPU host that has no docker, it stages
  the tree and compiles it with `run` inside the published SDK image. The target name is the stage's, so once that
  stage lands the arm can switch to buildx without moving anything else.
- **Five** cases for Stage 1, because Gate 3 asks a different question from the others — it characterises a hardware
  capability rather than exercising an injected library, and so needs no shim and can run first:
  (1) build the PoC shims and assert their linkage, no hardware; (2) inject and `ppu-smi` visibility, with the
  `dlsym`-less control arm; (3) VRAM quota enforcement across the memory paths; (4) the
  `hgmlDeviceGetProcessUtilization` characterisation; (5) multi-card per-device quotas.
  Stage 2 adds two more, for properties none of the five can reach: (6) `common/`'s unit tests plus two processes in
  one container against one quota, and (7) a fourth gate — compute throttling under a load that would otherwise take
  the whole card, which is the first case needing a real kernel and therefore the vendor's own compiler.
- **Container-runtime fallback**: `scripts/lib.sh` gains an `XB_CTR` runtime abstraction (probe `docker`, then
  `nerdctl`, including the namespace/socket a k3s-hosted containerd needs) and the new THead cases are written
  against it. Scope of the rewiring, measured rather than assumed: `preflight.sh` runs **no** container at all
  (its docker mentions are `buildx`, `info`, `version` and bare probes — its three `docker run` grep hits are the
  phrase "ascend/nvidia docker runtime" in a comment and two WARN messages), so its change is purely to the gate.
  `build.sh` extracts artifacts with `create` + `cp` + `rm` and likewise has no `docker run`, so it stays
  docker-only by construction. The 18 real `docker run` sites all live in the existing Ascend (13) and NVIDIA (5)
  case scripts and are left alone — they cannot be verified without that hardware, and converting them is a
  deliberate follow-up rather than part of this change.
- `preflight.sh` splits its gate: *run-capable* (`docker` or `nerdctl`) FAILs when absent because cases cannot
  run; *build-capable* (`docker buildx`) only WARNs, pointing at "build on a docker host, then load the image
  here". No buildkitd is started.
- Acceptance (**met**): `preflight.sh` on a docker-less PPU host reports run-capable PASS and build-capable WARN
  with `FAILS=0`; `XB_CTR=<nonexistent>` produces exactly one FAIL row and `FAILS=1`; and case 1 executes through
  `nerdctl` — all three observed, the first and third on the PPU host itself.

#### F5 — Injection contract and the usage surface

**Environment contract.** The quota reaches the container as environment variables, but it is *configured* as
resource requests: the user sets `Instance.Spec.Resources.AcceleratorSliced{Memory,Cores}Percentage`, the Pod
webhook folds those into `.sliced.{memory-percentage,memory-mib,cores-percentage}` limits, and the device-plugin
allocator converts them through `pkg/deviceplugin/helper.go` into the vendor's own variable names. Editing the
env on a Pod therefore changes nothing upstream — not the conversion and not the Kueue credit accounting.

| Variable | Meaning | Absent in a sliced container |
| --- | --- | --- |
| `HGGC_DEVICE_MEMORY_LIMIT_<i>` | per-card VRAM cap, MiB, `<i>` the **container-local** device index | falls back to the un-indexed figure |
| `HGGC_DEVICE_MEMORY_LIMIT` | VRAM cap for every card carrying no figure of its own, MiB (**as built, ship time**) | init error for those cards |
| `HGGC_DEVICE_SM_LIMIT_<i>` | per-card compute cap, percent (**as built, ship time**) | falls back to the un-indexed figure |
| `HGGC_DEVICE_SM_LIMIT` | compute cap for every card carrying no figure of its own, percent | init error for those cards (**as built, T15**) |
| `HGGC_SM_CONTROL_PERIOD_MS` | the compute controller's gating window | defaults to 100 |
| `HGGC_SM_CONTROL_STEP_MS` | how often the loop may step | defaults to 1000, the sensor's own settling time |
| `HGGC_SM_CONTROL_KP` / `_KI` / `_KD` | the loop's gains, in hundredths | default to 25 / 8 / 0 |
| `HGGC_SM_GRAPH_WEIGHT` | launches' worth of window a graph launch is charged | defaults to 1, i.e. off |
| `LIBHGGC_LOG_LEVEL` | verbosity of the shim's own stderr: `0` silent, `1` denials and errors, `2` also the load markers and the per-entry counter dump | defaults to `1` |
| `HGGC_LEDGER_PATH` | the cross-process usage region's file | defaults to `/dev/shm/vppu-ledger` |

- **The five controller knobs are tuning, not quota, and they fall back rather than refuse.** A mistyped gain still
  leaves a working loop, where a mistyped cap would leave no cap — so each is bounded and defaulted, and the values
  actually in force are printed once at load. They exist because the gains are deliberately not inherited from
  flexai's fitted triples, which leaves the hardware they run on as the only place they can be fitted; the defaults
  were chosen against a simulated card in `common/`'s unit tests and are a stable starting point, not a fit.
  `HGGC_SM_CONTROL_STEP_MS` is the exception that is not a preference at all: it is the driver's measured settling
  time, and lowering it to the window reproduces an oscillation across the whole range.
- The `HGGC_` prefix is the SDK's own env namespace — `HGGC_INJECTION64_PATH` already exists as the
  `CUDA_INJECTION64_PATH` analogue — so these sit in the vendor's namespace exactly as HAMi-core's
  `CUDA_DEVICE_MEMORY_LIMIT_<i>` and `CUDA_DEVICE_SM_LIMIT` do. This replaces Stage 1's single container-wide
  `VPPU_DEVICE_MEMORY_LIMIT_MIB`, which cannot express per-card figures at all.
- `<i>` is the **container-local** index, not the host ordinal: the SDK renumbers devices inside a container, so a
  container given one card node addresses it as `0` while the host ordinal only names `/dev/alixpu_ppu<N>`. This
  was measured, not assumed (see F2's defect notes).
- Per-card is not a rename. `hgMemAlloc` carries no device argument — it charges the current context's device — so
  the shim has to resolve that device at allocation time (`hgCtxGetDevice`, or `hggcGetDevice` on the runtime
  side) before it can pick a figure.
- **The shim needs a verbosity knob, and today it has none.** It prints unconditionally: a constructor line, a
  line per denial, and a counter dump at exit. In a real workload that is stderr on every refused allocation with
  no way to turn it off, and both existing backends already have this knob with the allocator injecting a quiet
  default while respecting a workload-declared value (`ContainerEnvDeclared`): HAMi-core's `LIBCUDA_LOG_LEVEL`
  defaults to `1` and GPUStack injects `0`; vcann-rt's `ENPU_LOG_LEVEL` defaults to `3` and GPUStack injects `1`.
  `LIBHGGC_LOG_LEVEL` takes the vendor-library-flavoured form of the first, so the container carries one word stem
  rather than two.
  - **The allocator should inject `1`, not `0`.** Copying the NVIDIA branch here would be wrong: HAMi's level 1 is
    per-*call* chatter, while ours is per-*denial* — rare, and the one line that answers "why was my allocation
    refused". `0` silences that too and exists only for a workload that wants absolute quiet.
  - **`2` is not optional decoration: the gate cases depend on it.** Case 2 proves an arm's library loaded from the
    constructor marker and reads `intercepted dlsym(`, and case 3 reads the counter dump — so a default-quiet shim
    with no way to raise the level would silence four of the five cases. They pin `LIBHGGC_LOG_LEVEL=2`. Case 1 is
    unaffected: it greps the strings in the built object, not runtime output, so the strings must exist in the
    binary at every level.
- **The preloads are ordered, and the visibility shim must come first (as built, T17).** Two libraries that
  interpose `dlsym` do not chain through each other — the versioned `dlvsym(RTLD_NEXT, …)` lookup a `dlsym` hook is
  forced to use steps over an unversioned definition — so the loader gives the symbol to whoever it reaches first
  and the one behind it is loaded, initialised and never entered. Behind a peer this shim is inert and `ppu-smi`
  reports the physical card, which is a silent wrong figure rather than an error. Nothing in the library can detect
  or fix that from the inside, so it belongs to whoever writes the container's `/etc/ld.so.preload`: our entries go
  first, and a workload image that ships its own `dlsym` interposer is a case to notice rather than to trust. Case 2
  pins both directions, so the constraint is a checked fact.
- The `[vppu]` log tag stays as it is, and the deviation is deliberate: HAMi-core tags with its *project* name
  (`[HAMI-core Msg …]`, never `vgpu`), where `[vppu]` is the library name. Short and unique matters more here than
  matching that choice, because every case decides its rows by grepping it out of output interleaved with the
  vendor's own.
- **Both quota dimensions are an init error when absent, and the allocator must inject them explicitly** — each
  either per card or through its own un-indexed fallback. The helper asymmetry
  makes this load-bearing: `SlicedMemoryMib` errors when neither memory request is present
  (`pkg/deviceplugin/helper.go:143`) but `SlicedCoresPercent` **defaults to 100** (`:121`), so a compute figure
  that never gets injected silently means "a whole card's compute". The shim must not reproduce that default.
  - **The compute half was staged, and T15 closed it.** T13 latched "unusable, refuse every allocation" on the
    MEMORY figure only: an absent or unparsable compute figure was reported at the denial level and nothing more,
    because refusing every allocation over a dimension the library did not yet implement would have failed closed
    on the wrong thing. **As built (T15) both figures decide it**, so a sliced container carrying a memory figure
    and no compute figure is refused outright — which is why every case that injects the shim now injects
    `HGGC_DEVICE_SM_LIMIT` too, at `100` where compute is meant to be uncapped.
  - **Both dimensions carry both forms, and the precedence is the point.** `HGGC_DEVICE_<dimension>_LIMIT_<i>`
    decides card `<i>` wherever it is **set**, and every other card reads the un-indexed form — the order HAMi-core
    reads `CUDA_DEVICE_{MEMORY,SM}_LIMIT{,_<i>}` in, so a container sliced by an allocator that knows one figure
    per dimension keeps running. Stage 1 had the indexed memory figure alone and the un-indexed compute figure
    alone; ship time made the pair symmetric, because two conventions for two dimensions is not a contract.
    What is deliberately **not** HAMi's is where the search stops: being set stops it, so a figure that is set and
    malformed (or, for compute, above 100) makes that card unusable rather than falling through to the level above
    and then, for compute, to a default of 100 — which would turn a typo into a whole card's compute.
    With only the un-indexed forms set, EVERY card is a card the container was given — nothing in the environment
    says how many it holds — so that pair is a complete configuration and both halves of it must be usable. The
    region needed no change for any of this: `sm_limit` was already a per-card word, previously carrying one figure
    copied into every slot. **GPUStack's own NVIDIA allocator emits only the un-indexed `CUDA_DEVICE_SM_LIMIT`**,
    so the THead allocator handoff (F3) is where per-card compute becomes requestable; until then a two-card
    container can only be given two different compute caps by hand.

**Usage surface.** Two reference implementations answer this differently, and the difference decides the design:

| | HAMi-core | flexai `xpu-pool-service/direct` |
| --- | --- | --- |
| Ledger | file-mapped cross-process region (`/tmp/cudevshr.cache`, 16 device slots + 1024 process slots, magic `19920718`), process-shared POSIX semaphore + seqlock + `pthread_atfork` | **stateless** — queries the vendor library per allocation; the only sync is `flock(LOCK_EX)` on `/run/xpu/memctl.lock`, held by an RAII guard until the real allocation returns |
| Machine-readable usage | **yes** — the region is the surface an external monitor reads | **none** — the documented answer is a separate CLI, `/opt/xpu/bin/gpu-monitor` |
| Known trap | the limit is written only by the region's **first creator**, so a stale cache freezes the old limit and changing it means deleting the file | a missing config file is read as "not in a container" and imposes **no limit at all** |

So a stateless ledger is rejected for this purpose specifically: there is nothing to scrape. `common/` takes
HAMi's region structure with flexai's lock semantics, and avoids both traps — the quota is re-read rather than
frozen, and a missing or unparsable quota is an init error.

- The region serves **three** consumers, which is why its layout carries a magic and a version and is documented
  as a contract: the cross-process quota itself, `tools/`'s human-readable output, and a future metrics scraper.
- **As built (T13), layout version 1.** Magic `VPPUREGN` at offset 0 — eight ASCII bytes rather than a number so
  `strings` on the file identifies it — then the version at 8, the header size at 12 and the slot counts at 16 and
  20. Byte order is the host's, which costs nothing because writer and readers are processes in one container on
  one machine. Per card: the quota in force, the accounted total, the compute limit, the measured utilisation, a
  reserved area for the controller's state, and up to 32 per-process charges. The aggregate and the per-process
  breakdown are both kept on purpose: the aggregate makes an admission one read, and the breakdown is what lets a
  dead process's charge be identified and dropped.
- **The lock is an `fcntl` record lock on one byte per card**, taken from a 64-byte arena whose offset (32) is
  frozen at version 1 — two builds speaking different versions must still take the same byte for the same card, or
  they lock different offsets and exclude nobody. `fcntl` rather than a `pthread_mutex` or a `sem_t` in the region
  because both of those need `-lpthread` on the glibc 2.17 floor, which would put a second name in `DT_NEEDED` and
  break the guarantee case 1 asserts; per card rather than `flock`, which locks the whole file and therefore the
  whole container. A record lock is per process, so an in-process spinlock built from GCC atomics covers sibling
  threads, and re-entry on the same card is counted rather than deadlocked — the vendor's allocation runs under
  this lock and may call back into an interposed entry.
- **`fork()` duplicates that spinlock, and the same constraint rules out the usual fix.** A child inherits the
  flag as *held* by a thread that does not exist in it, and would wait on it forever; it also inherits the
  thread-local "I hold card 0" state and would count an inherited lock as its own re-entry, skipping the lock
  entirely. `pthread_atfork` is the standard answer and is unavailable for the same `-lpthread` reason, so the
  process-local state is stamped with its owning pid and reset on first use in a new process. This is not an
  exotic shape: a data loader started with the fork method does it every epoch, from a process whose other
  threads are allocating. The *mapping* is deliberately not reset — it is `MAP_SHARED` on an inherited
  descriptor, so it is still the same region — and record locks are not inherited, which is what makes clearing
  the in-process flags safe: the child then blocks on the parent's record lock exactly as another process would.
- **A process that dies holding a charge does not shrink the quota forever.** Its slot is swept — by liveness, on
  the path that would otherwise refuse, so the sweep costs nothing on the hot path — and the card's total is
  re-derived from the live slots. Without this, HAMi-core's stale-cache problem arrives by another route: a killed
  training process would hold its bytes for as long as the region file lives.
- **The key → (card, bytes) map stays process-local, and must.** A device pointer is a value in one process's
  address space, so two processes can hold the same one on different cards; a shared table keyed by it would let
  one process's free refund another's allocation. What has to be shared is the total, and that is what the region
  carries.
- It must carry, per card: the quota and the accounted total; per process: its charge; and for compute: the
  configured limit, the measured utilisation, and the PID loop's own state. The last one is not decoration — with
  gains that are env-tunable and deliberately not inherited from flexai's fitted values, an unobservable loop
  cannot be tuned on unfamiliar hardware.
- The compute **limit** appears nowhere else. `ppu-smi` has no maximum-SM field, exactly as `nvidia-smi` has none,
  so without this surface a compute quota can only be inferred from an init log or a stress test.
- **Not through `Devices.Status`.** That is rebuilt wholesale each reconcile from the Spec plus Pod annotations
  with no live query, so live usage cannot ride it.
- The scraper itself is **out of scope**, deliberately: GPUStack has no per-slice usage metric today — `/metrics`
  carries the worker-gateway's controller-runtime and client-go collectors, Ascend's `enpu-monitor` is mounted
  into the container for a human to run and nothing reads it from outside, and HAMi's `vGPUmonitor` is not wired
  in. THead is the first backend that could have one, so this spec fixes the contract it will read and stops
  there.
- Acceptance: `tools/` prints both dimensions' quota and usage for every card the container holds; the region's
  layout is documented with its magic and version; and a reader outside the shim can parse it from the mounted
  ledger directory with no access to the shim's own symbols.

### Notes / Constraints / Caveats

- **Deployment model.** The PPU SDK lives in the workload container; the host passes the device nodes the THead
  allocator already mounts — the two control devices `/dev/alixpu` (no underscore) and `/dev/alixpu_ctl`, plus
  `/dev/alixpu_ppu<N>` per allocated card (`pkg/devicemanager/allocator/thead/deviceplugin.go:106-129`). The whole
  user-space stack resolves with zero missing libraries in a driverless container, so no host library injection
  and no `runtimeClassName` are required — unlike NVIDIA (host-injected `libcuda.so.1` /
  `libnvidia-ml.so.1`) and unlike Ascend (link-time `dcmi` stubbing).
- **glibc floor is a hard constraint, not a convenience.** The shim loads inside a container we do not control,
  so it must be built on an old base. This is independent of the operator image's own Ubuntu 24.04 base.
- **Existing vendored libraries.** `libhgml.so` (2.1 MB) and `libuki.so` (702 KB) are already committed under
  `pack/gpustack-operator/rootfs/lib/x86_64-linux-gnu/gpustack/` via **Git LFS** and copied into the image;
  they serve the device-manager's `dlopen` path so it need not carry a multi-GB SDK. Any builder must have LFS
  content available — `pack.yml` already checks out with `lfs: true`.
- `hgml.h` is not C-clean: it uses a bare `bool` in exactly one declaration
  (`hgmlDeviceDestroyVgpuInstance(..., bool force)`) without including `<stdbool.h>`. Plain C compilation fails;
  `-include stdbool.h` or C++ succeeds. HAMi-core is a C project, so a port hits this on the first build.
- `hgmlMemory_t` is `{total, free, used}`; `hgmlMemory_v2_t` is `{version, total, reserved, free, used}` and the
  caller-supplied `version` must be written back unchanged.
- `HGGC_INJECTION64_PATH` exists (the `CUDA_INJECTION64_PATH` analogue) and HGPTI offers driver/runtime callback
  domains, but HGPTI callbacks return `void` with a `const` payload — usable for metering, not for enforcement.
- **SDK layout, as observed inside `gpustack/thead-ppu-devel:2.1.1`** (digest `sha256:5f83fd14…`), because the
  older `ppu-sdk-1.0` checkout is not representative — it contains only `targets/`: the top level is
  `LICENSE NOTICE VERSION.txt asight bin cfgs envsetup.sh include lib ppu-smi release.yaml targets`, and
  `VERSION.txt` is two lines, `ppu_sdk_detection_magic` and `hggcrt_version:v3`. There are three `bin`
  directories: `bin` (compiler toolchain only — `hgcc`, `hglink`, `llvm-*`, `ppu-llc`, `hggc-memcheck`),
  `ppu-smi/bin`, and `asight/bin` (profiler suite). The image originally put only the first on `PATH`, so it
  shipped `ppu-smi` and hid it; [#75](https://github.com/gpustack/gpustack-operator/pull/75) adds
  `${PPU_HOME}/ppu-smi/bin` and a `command -v ppu-smi` smoke assertion. A published tag only carries that after
  the pack workflow is re-dispatched, so anything verifying visibility should still invoke `ppu-smi` by absolute
  path.
- **`ppu-smi` exits 0 even when it fails.** Measured with no PPU present: it prints
  `init HGML error: driver is not loaded` and returns 0. Its exit status is therefore not a success signal — every
  check must parse its output. Its `ldd` is also clean and names no HGML library, which is the linkage-level
  confirmation that it reaches HGML through `dlopen` at runtime.
- Version axes do not map onto each other: package `2.1.1`, SDK directory `ppu-sdk-1.0`,
  `HGGCRT_VERSION 13000`, API generation `v3`. `HGGC_SDK_VERSION` is the placeholder `"0.0.0-000000"` and must
  not be used for detection.
- Test-host reality: it runs rke2 with production inference workloads and one card already holds ~91 GB, so
  hardware cases must pick an idle card rather than a fixed index.

### Boundaries

- **Always:** keep every preload container-scoped; keep `libvppu.so`'s `NEEDED` limited to `libc.so.6`; assert
  the SDK generation at build time and fail closed; pick an idle card for hardware cases; record a failed PoC
  gate as a finding rather than retrying silently.
- **Ask first:** starting, installing, or reconfiguring any daemon on a user-owned host (buildkitd included);
  building images on the PPU host rather than on a docker host; anything that consumes a card another workload
  is using; publishing an image or opening a PR.
- **Never:** write the host's `/etc/ld.so.preload`; replace or shadow a host library file; commit or log a
  presigned `PPU_SDK_URL`; commit the SDK archive; let a missing quota config degrade into "no limit"; inject the
  slicing preload into the device-manager process.

### Risks and Mitigations

- ~~`ppu-smi` visibility fails on real hardware despite the mechanism analysis~~ → **retired by Gate 1**: the
  hook made `ppu-smi` report 4096 MiB against a measured 98304 MiB baseline, and the `dlsym`-less control arm
  showed the physical figure while proving it had loaded.
- ~~The `dlsym` hook recurses or deadlocks against the vendor's `libhggc_wrapper.so`~~ → **retired by Gate 1
  arm (c)**: with the wrapper preloaded alongside the hook, `ppu-smi` still reported the quota, timeout-bounded,
  with no recursion. The prior reasoning also held — the wrappers exist but **no library lists them in
  `DT_NEEDED`** (87 ELFs scanned), so they are opt-in. The re-entrancy/origin guard stays on the `hgml/` module's
  list anyway: arm (c) exercises one wrapper, not every ordering a workload might set up.
- ~~Driver-layer-only interception proves incomplete because a path reaches `libalippu.so` directly~~ →
  **retired by Gate 2**: all four paths were refused with the marker and each named the driver entry that fired,
  so none bypassed `libhggc.so`. Escalation to the UMD layer is not needed for these paths.
- ~~`hgmlDeviceGetProcessUtilization` is unsupported at runtime or reports host PIDs~~ → **retired by Gate 3**:
  supported, non-empty under load, and the reported `pid` is the container's own, so the loop takes per-process
  feedback and the card-total fallback is not needed.
- The PID loop oscillates on unfamiliar hardware → gains are env-tunable and the loop state is logged and
  surfaced in the reader tool, so it is diagnosable instead of a black box.
- A compute quota is configured, injected, and silently ignored → the shim treats a card with no compute figure
  (neither `HGGC_DEVICE_SM_LIMIT_<i>` nor the un-indexed fallback) as an init error rather than inheriting
  `SlicedCoresPercent`'s default of 100 — and refuses a figure that is set and malformed rather than falling
  through to the level above, which is where HAMi-core lands back on that same 100. The asymmetry is real and easy to
  miss: the memory helper errors when nothing is requested, the compute helper returns a whole card's worth, so
  the only place this can be caught is the shim.
- A `fork()` in a multi-threaded workload duplicates a held in-process lock, so the child either waits on a
  holder that does not exist or counts the inherited lock as its own re-entry and bypasses the quota → the
  process-local state carries the pid that owns it and is reset on first use in a new process, since
  `pthread_atfork` would breach the `libc.so.6`-only linkage rule. Both failure modes are covered by one unit
  test that forks while the parent holds a card, and it was verified to fail without the reset (`child verdict 1`
  — let through while the parent still held it).
- The usage region becomes a de-facto public API and then cannot be changed → it carries a magic and a layout
  version from T13, T18 documents the layout as a contract, and T13's tests include a reader at an older version
  refusing rather than misparsing. The alternative — a scraper written against an undocumented struct mmap — is
  what makes the next added field a breaking change.
- The region's lock is held across a vendor call that hangs, wedging every process in the container → the lock is
  per card rather than per container, its holder writes its pid into the region so `tools/` can name it, and the
  ledger path takes no lock at all on the read side. Held-until-the-allocation-returns is the semantics that
  closes check-then-alloc; the cost is that a hung driver call is now also a hung container, and that trade is
  taken deliberately because the alternative is a quota that can be exceeded under concurrency.
- Compute throttling lands but cannot be verified because no THead workload saturates a card → case 7 supplies
  its own load, as the Gate 3 probe already does; its `smUtil` rose 0 → 6 → 12 under nothing more than a copy
  loop, so a saturating kernel is a small extension of code that already runs on this hardware.
- A presigned `PPU_SDK_URL` shows up in a build log → the SDK stage keeps tracing off across every command that
  reads it (#74; F1's first build did trace it). The link is not treated as a secret — it is a rotating download
  URL for a vendor archive, not a credential — so this is log hygiene, not an incident. Mirroring the archive to
  a stable internal location remains the follow-up that retires the whole `ARG` dance.
- ~~An allocation that names its own card is charged to the calling thread's context instead~~ → **fixed at ship
  time**, after the cross-model review found it: the VMM path carries `prop->location.id` and a pool allocation
  belongs to its pool's card, and neither has to be the current context, so a container holding two cards could
  take memory from one and quota from the other. Both now admit against the card they name
  (`vppu_hggc_admit_on`), a pool's card is remembered at `hgMemPoolCreate` and a **default** pool — which never
  passes through it — is resolved by asking the vendor for each held card's default pool and comparing handles. A
  named card above the layout's bound is refused rather than folded back onto the context. Two case-6 Part C rows
  pin it, both verified to fail against the pre-fix code.
- ~~A file that is not a region is grown before it is refused~~ → **fixed at ship time**, same review: the magic
  was checked after an `ftruncate` that had already resized somebody's file to 36960 bytes. The short file that may
  be grown is now only an empty one or one already opening with our magic, and the unit test asserts the foreign
  file's **size** as well as its content (12 bytes, was 12 — it read 36960 before the fix).
- An asynchronous free is refunded when it is enqueued rather than when it completes → **accepted and recorded**,
  not fixed. `hgMemFreeAsync` returning success means the free is queued behind that stream's outstanding work, so
  the ledger gives the bytes back while the memory is still live and the container can transiently hold more than
  its quota. Fixing it properly needs a per-stream deferred queue or a completion callback through
  `hgLaunchHostFunc` — a design change rather than a correction — and the overshoot is bounded by what the driver
  will actually hand out, since a real allocation beyond the physical card still fails and rolls back. Follow-up.
- A process that launches kernels but never allocates is left out of the compute feedback → **accepted and
  recorded**, not fixed. The controller tells its own container's utilisation samples from a neighbour's by asking
  whether the pid holds a slot in the card's process table, and a worker running on imported or shared memory has
  no slot, so its share is not measured and the container's aggregate can exceed the cap. The fix is a second
  membership signal — registering a pid on its first launch rather than its first allocation — which changes what
  the process table means and can exhaust its 32 slots, so it is a design decision. Follow-up.
- A THead detector sets per-card `Status.LogicalSliced` but the group-level aggregate stays empty → the handoff
  requires **both** the per-card field and the `device.SetGroupSlicedDetails(grpList)` call that all six
  slicing-capable vendors make (`pkg/device/sliced.go:37`) and the THead detector currently does not
  (`pkg/devicemanager/detector/thead/device.go:202` returns without it). Nothing fails loudly: pool capability is
  `SlicedDetail.Logical.Count > 0` (`api/worker/v1alpha1/instance_type.go:209-211`), so a zero aggregate simply
  makes the pool un-sliceable and the chain never materialises slices.
- `nerdctl` does not cover every `docker` subcommand the cases use → the runtime abstraction only has to carry
  `run`; artifact extraction (`create` + `cp` + `rm`) lives in `build.sh`, which stays docker-only by
  construction because it is the build-capable path.
- A new case script is written but cannot be invoked → `SKILL.md`'s `allowed-tools` list is per-script with **no
  glob**, so every added case needs its own explicit entry, and any container command the skill has Claude run
  directly needs one too (today only `docker buildx|images|info` are listed).
- The arm64 leg of `pack.yml` fails for an amd64-only SDK → the matrix becomes per-image before
  `thead-ppu-devel` is added to the choice list.
- A future SDK generation breaks the shim → the build-time `VERSION.txt` gate fails the build rather than
  shipping a silently mismatched library.

## Design Details

### Commands

**Environment.** Everything this spec builds — shell scripts, the C shims, spec text — builds and verifies
**locally** on the development machine; the shims are compiled *inside* the amd64 devel image, so an arm64 dev
host is fine with an explicit `--platform`. The hardware cases target a **remote PPU host over SSH** and have
**run**: all three gates PASS there (results under F2). That host has no docker, so `XB_CTR` resolves to
`nerdctl`, and it runs rke2 whose containerd keeps the images in the `k8s.io` namespace.

```bash
# ---- devel base image (F1, delivered) ----
PPU_SDK_URL='<url>' make package thead-ppu-devel     # local build, amd64 host
# publish: GitHub Actions workflow_dispatch, repository=thead-ppu-devel,
#          tag=<free-form, as with ssh-server>, args=PPU_SDK_URL=<url>

# Check the Docker VM's backing volume before any large pull or build: the VM's own `df`
# misreports a sparse Docker.raw, and filling that volume corrupts the VM.
df -h /System/Volumes/Data

docker run --rm --platform linux/amd64 gpustack/thead-ppu-devel:2.1.1 \
    bash -c 'cat "${PPU_HOME}/VERSION.txt"; command -v ppu-smi'

# ---- this spec's own verification ----
# No shell linter is wired into this repo -- hack/lint.sh covers Go (golangci-lint + goimports) and
# the helm chart only, and no CI job runs shellcheck -- so `bash -n` plus each case's own FAILS=
# contract is the gate the repo enforces. shellcheck is still worth running by hand, because these
# scripts carry `# shellcheck disable=` directives and so presume a reader that honours them; -x
# follows the `. lib.sh` source, which is where the word-splitting the directives excuse lives.
bash -n .claude/skills/gpustack-operator-xbuild-and-verify/scripts/*.sh \
        .claude/skills/gpustack-operator-xbuild-and-verify/cases/*.sh
shellcheck -x -S warning .claude/skills/gpustack-operator-xbuild-and-verify/scripts/*.sh \
                         .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-*.sh

bash .claude/skills/gpustack-operator-xbuild-and-verify/scripts/preflight.sh
XB_CTR=nosuchruntime bash .claude/skills/gpustack-operator-xbuild-and-verify/scripts/preflight.sh  # expect FAILS=1
bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-1.sh                      # hardware-free

# ---- regression: unchanged by this spec, run to prove it ----
make lint          # whole-module golangci-lint + goimports; EDITS files; slow on a cold cache
make test          # go test -failfast -race -cover -timeout=30m; any args are EXCLUSION regexes
make lint chart    # offline chart checks (NOT `make test chart`, which mutates a live cluster)

# ---- hardware cases on a PPU host (all three gates PASS; see F2) ----
# That host runs rke2 with production inference on 16 cards, one holding ~91 GB, so cases must
# pick an idle card; and docker.io times out there, so the image comes in through the cluster's
# own mirrors: `crictl pull docker.io/gpustack/thead-ppu-devel:2.1.1` goes through CRI and lands
# in the k8s.io namespace, where nerdctl can run it. `nerdctl pull` does NOT read containerd's
# config.toml mirrors and would hang.
# XB_SSH_OPTS carries any extra ssh options; a target reachable only through a bastion needs
# '-J <user@bastion>' there, otherwise every hardware case has to be run by hand.
export XB_MODE=ssh XB_HOST=<user@host> XB_CTR=nerdctl XB_CTR_ARGS='--namespace k8s.io'
bash .claude/skills/gpustack-operator-xbuild-and-verify/scripts/preflight.sh
# First, and again after any source edit: stages the tree and compiles every artifact in the SDK image.
bash .claude/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh xbuild-thead-ppu
bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-1.sh   # build + linkage
bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-2.sh   # Gate 1
bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-3.sh   # Gate 2
bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-4.sh   # Gate 3
bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-5.sh   # multi-card quotas
bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-6.sh   # common/ unit tests + one
                                                                               #   quota, two processes
bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-7.sh   # Gate 4: compute throttling
```

`common/`'s unit tests are the one C artifact here that needs neither hardware nor the SDK image, so they can also
be built and run straight on a development machine while iterating — which is what case 6 does inside the image:

```bash
cd csrc/thead/ppu-slicing-shim
./build.sh unit && ./vppu_test   # the same STATUS | CHECK | DETAIL rows, ending in FAILS=<n>
```

The same script builds everything else, and is the only place the translation-unit lists and flags live —
`./build.sh lib | test | check v1 | list <artifact>`. `lib` and `test` need the SDK's headers, so they run inside
`gpustack/thead-ppu-devel:2.1.1` (or on a host with the SDK installed); `unit` needs neither, which is the whole
point of `common/`. See `csrc/thead/ppu-slicing-shim/README.md`.

### Project Structure

Delivered by F1, already in `main`:

```
pack/thead-ppu-devel/Dockerfile        # PPU SDK devel base -> gpustack/thead-ppu-devel:2.1.1  (#73, #74, #75)
pack/ssh-server/                       # the existing precedent for an auxiliary pack target
hack/package.sh                        # conditional --build-arg PPU_SDK_URL
.github/workflows/pack.yml             # thead-ppu-devel choice, build-arg override input, per-image matrix
```

This spec's own deltas — note `.claude/skills/` **is** version-controlled (`.gitignore:40` re-includes it after
`.claude/*`), so these are tracked changes like any other:

```
csrc/                                  # NEW: this repo's first C source tree (binding/ holds only CGO bindings)
└── thead/ppu-slicing-shim/            # the libvppu.so source tree -- PRODUCT code at this level
    ├── README.md                      # STAGE 2: how to build the shims and how to run one by hand
    ├── build.sh                       # STAGE 2: the ONE place that knows how to compile this tree --
    │                                  #      lib | test | unit | check v1 | list. Silent on success, because
    │                                  #      case 1 decides "compiles clean" on empty output
    ├── common/                        # STAGE 2, delivered: no hg*/hggc*/hgml* type may appear here, which
    │   ├── vppu.h                     #      is what makes it testable without the SDK or a device
    │   ├── vppu_log.c                 #      the verbosity level
    │   ├── vppu_quota.{h,c}           #      env parsing, the load-time report, the usable/unusable latch
    │   ├── vppu_ledger.{h,c}          #      the versioned region, the per-card lock, charges, the key map
    │   ├── vppu_pid.{h,c}             #      the compute controller's arithmetic -- the one half of the loop
    │   │                              #      that needs no card, so the one half that can be unit-tested
    │   └── vppu_test.c                #      this library's first unit tests -- run by case 6, no device
    ├── hggc/                          # STAGE 2, delivered: both quotas over the whole driver layer --
    │   ├── hggc_quota.h               #      54 interposed names in one shared object
    │   ├── hggc_quota.c               #      the memory admission decision, the entry table, the counters
    │   ├── hggc_mem.c                 #      the current-ABI memory entries: charge / refund / report / count
    │   ├── hggc_mem_v1.c              #      the v1 ABI names, whose parameter types differ
    │   ├── hggc_entry.c               #      hgGetProcAddress{,_v2} and hgGetExportTable
    │   ├── hggc_compute.c             #      the duty-cycle window, the loop, HGML sampling through dlopen
    │   └── hggc_launch.c              #      the 16 launch entries: gate / gate-as-graph / count
    ├── hgml/                          # STAGE 2, delivered: visibility, one shared object
    │   └── hgml_dlsym_hook.c          #      the dlsym hook, both memory getters, the two guards; `used`
    │                                  #      comes from common/'s ledger, so both halves show one number
    ├── tools/                         # STAGE 2, delivered: preloaded into nothing -- it reads
    │   └── ppu_monitor.c              #      -> ppu-monitor: both quotas and both usages per card,
    │                                  #      including the compute limit ppu-smi has no field for;
    │                                  #      links none of common/, maps the region read-only itself
    └── testing/                       # gate-only artifacts, never shipped in the library
        ├── hgml_nohook.c              # NEW: Gate 1's negative control -- same HGML symbols, no dlsym hook
        ├── dlsym_origin.c             # STAGE 2: Gate 1's mechanism probe -- dladdr says which object won a
        │                              #      symbol; was 28 lines of C inside case 2's heredoc
        ├── dlsym_stack.c              # STAGE 2: a SECOND dlsym interposer, stacked against the hook in
        │                              #      both preload orders -- how the ordering constraint was measured
        ├── hgml_util_probe.c          # NEW: hgmlDeviceGetProcessUtilization + PID-namespace characterisation
        ├── hggc_mem_paths.c           # NEW: Gate 2's workload half -- one memory path per invocation,
        │                              #      plus the hold path case 6 contends against
        └── hggc_launch_load.cu        # NEW: Gate 4's workload half -- the only file in the vendor's device
                                       #      dialect, built by hgcc, because a kernel is the only way to
                                       #      occupy a PPU; reports its own per-process smUtil
.claude/skills/gpustack-operator-xbuild-and-verify/
├── scripts/lib.sh                     # + XB_CTR runtime resolution (docker -> nerdctl, + XB_CTR_ARGS),
│                                      #   + XB_SSH_OPTS for a jump-host target
├── scripts/build.sh                   # + an xbuild-thead-ppu arm: stage the shim tree, compile it inside
│                                      #   the SDK image by calling the tree's own build.sh
├── scripts/preflight.sh               # + split run/build-capable gate, + three THead hardware WARN rows
├── cases/thead-case-{1..7}.sh         # NEW
├── references/thead-*.md              # NEW ×2, + a THead section in troubleshooting.md
├── references/thead-hggc-symbol-manifest.md   # NEW: the F3 symbol surface, with the image digest it came from
└── SKILL.md                           # + third Cases table, THead env knobs, per-case allowed-tools entries
specs/2026-08-03-thead-ppu-slicing-shim.md
```

Handed off — the `pack/` and operator-side work Non-Goals excludes, listed so the boundary is explicit:

```
pack/gpustack-operator/Dockerfile      # + xbuild-thead-ppu stage (FROM gpustack/thead-ppu-devel:2.1.1),
                                       #   + COPY to ${GPUSTACK_LIB_DIR}/thead/, + an install -D for the
                                       #   THead ld.so.preload beside the ascend/nvidia pair at :471-474.
                                       #   No ARG LIB_<x>_COMMIT: unlike HAMi-core, the source is in-repo
                                       #   under csrc/, so there is no upstream commit to pin.
pack/gpustack-operator/external/thead/build-libvppu.sh
pack/gpustack-operator/rootfs/etc/gpustack/lib/thead/ld.so.preload
pkg/devicemanager/detector/thead/device.go          # + LogicalSliced, gated the way Ascend gates it
                                                    #   (detector/ascend/device.go:497-524), + the
                                                    #   device.SetGroupSlicedDetails(grpList) call it lacks
pkg/devicemanager/allocator/thead/deviceplugin.go   # + Sliced server behind !opts.NoSliced, + injection branch
                                                    #   emitting HGGC_DEVICE_MEMORY_LIMIT_<i> and
                                                    #   HGGC_DEVICE_SM_LIMIT_<i> per card, plus
                                                    #   LIBHGGC_LOG_LEVEL=1 guarded by
                                                    #   ContainerEnvDeclared. It must inject the compute figure
                                                    #   even at 100%: SlicedCoresPercent defaults to 100, so
                                                    #   omitting it is indistinguishable from "no compute quota"
```

### Code Style

Dockerfile conventions follow `pack/gpustack-operator/Dockerfile`: `ARG`s at the top, a heredoc `RUN` whose body
is **verbatim** (the shell does all expansion — never backslash-escape `$` inside it), `set -exo pipefail`, and a
comment that states *why* rather than *what*. `pack/thead-ppu-devel/Dockerfile` is the delivered exemplar; the
one convention it adds is that a stage handling a secret defers `set -x` until no command reads it:

```dockerfile
FROM base AS sdk
ARG PPU_SDK_URL

RUN <<EOF
    # Tracing stays OFF across every command that reads the URL. `set -x` prints every
    # expanded word, so tracing the emptiness guard or the curl publishes the presigned
    # link -- signature included -- into a world-readable CI build log. PPU_SDK_URL is an
    # ARG, so it stays set for the whole stage; what makes the re-enable safe is that no
    # command after it reads the URL.
    set -eo pipefail

    if [[ -z "${PPU_SDK_URL}" ]]; then
        echo "[ERROR] PPU_SDK_URL is required." >&2
        exit 1
    fi
    curl -fsSL --retry 3 --retry-delay 5 -o /tmp/ppu-sdk.run "${PPU_SDK_URL}"

    set -x
    # Self-extract instead of running install-ppu.pl, which needs the perl module Tie::File
    # that a clean Ubuntu base (perl-base only) lacks; it exits 2 before creating anything.
    sh /tmp/ppu-sdk.run --noexec --keep --target /tmp/ppu-sdk-extract >/dev/null
    mv /tmp/ppu-sdk-extract/PPU_SDK /out
    grep -q "hggcrt_version:${PPU_SDK_EXPECT_RT}" /out/VERSION.txt
EOF
```

The download lives in a stage that is never pushed, because BuildKit writes every build argument's value into the
image history of the stage that declares it — declaring `PPU_SDK_URL` in the final stage would publish the
presigned link to anyone who pulls the image.

Go conventions are the project's existing ones (`docs/development.md`, `CLAUDE.md`): snake_case multi-word file
names, explicit error handling, level-based reconcile. Shell in the skill follows the existing case scripts: a
`row()` three-column printer, a `fails` counter, `echo "FAILS=${fails}"`, and an outer `grep -q 'FAILS=0'`
deciding PASS/FAIL and the exit code.

C is new to this repository — `binding/` holds only generated CGO bindings, so `csrc/` establishes the
convention: snake_case file names as in Go, one shared object per interposed library — built from as many
translation units as the module needs, so the linkage assertions stay per-artifact — and no include of `hgml.h`
or `hggc.h` without `<stdbool.h>` and `<stddef.h>` ahead of it. A module spanning translation units declares its
own seam in a private header and marks every entry `VPPU_INTERNAL` (hidden visibility): those symbols have
external linkage now, and a preloaded library that exported them would be interposable by the very workload it
was sent to police. Case 1 asserts that rather than trusting it.

Interposing a versioned SDK has three idioms worth stating once, because T14 and T15 needed all three and T17 will:

```c
/* 1. Write the PLAIN source name to define the versioned symbol: hggc.h maps hgMemAlloc onto
 *    hgMemAlloc_v2, so this defines hgMemAlloc_v2 AND has the header type-check every parameter. */
HGresult HGGCAPI hgMemAlloc(HGdeviceptr *dptr, size_t bytesize)
{
    int device = -1;

    vppu_hggc_count(VPPU_MEM_ALLOC);
    if (!vppu_hggc_admit(VPPU_MEM_ALLOC, bytesize, &device)) {
        return HGGC_ERROR_OUT_OF_MEMORY;   /* a refusal is fail-closed, never a pass-through */
    }
    /* ... call the vendor through vppu_hggc_next(), then commit or roll back ... */
}

/* 2. Take a suffixed variant's type FROM the plain entry instead of retyping it, so a signature
 *    that ever diverges fails the build rather than corrupting a call. */
extern __typeof__(hgMemAllocAsync) hgMemAllocAsync_ptsz;

/* 3. #undef the mapping to reach the plain exported name -- in a translation unit of its own, so
 *    no file both keeps and cancels the mapping depending on where a definition happens to sit. */
#undef hgMemAlloc
HGresult HGGCAPI hgMemAlloc(HGdeviceptr_v1 *dptr, unsigned int bytesize) { /* the v1 ABI */ }
```

The entry table is the single source of truth: the ABI names live in one array beside the enum with a
`_Static_assert` on the two lengths, because a name appended to only one of them shifts every entry after it and
the wrappers would then silently count and resolve the wrong symbol.

**The compile recipes live with the sources, in `csrc/thead/ppu-slicing-shim/build.sh`** — the translation-unit
lists, the include roots, which artifact links the SDK and which may not. A verification case may *invoke* it and
judge the result; it may not carry a compiler invocation of its own. Two properties make that work: the script is
**silent on success**, so a case can decide "compiles clean" on empty output rather than on an exit status that a
warning would not change, and it takes no view on containers — the caller decides whether it runs inside the SDK
image or on a host that has the SDK.

Linkage splits by directory, and that split is the reason `testing/` exists rather than a naming convention:

- **`csrc/thead/ppu-slicing-shim/*.c` — shipped, and may link nothing but libc.** The product is preloaded into a
  container that brings its own SDK, so `DT_NEEDED` stays empty or `libc.so.6` and every vendor symbol is
  resolved at runtime through the `dlsym` chain. That is why the shims carry no `-ldl` either: `dlsym` and
  `dlvsym` stay undefined and bind to whatever glibc the workload has.
- **The libc-only rule reaches further than the link line, and two things fell out of it in T13.** A
  `pthread_mutex_t` or a `sem_t` in the shared region — the obvious way to write a process-shared lock, and the way
  HAMi-core writes it — needs `-lpthread` on the glibc 2.17 floor, so the lock is an `fcntl` record lock instead.
  And a plain `__thread` variable in a shared object uses the general-dynamic TLS model, which resolves through
  `__tls_get_addr` and puts **`ld-linux-x86-64.so.2`** in `DT_NEEDED`: case 1 caught it, and the fix is
  `__attribute__((tls_model("initial-exec")))`, which is correct here precisely because this library only ever
  arrives through `LD_PRELOAD` or `/etc/ld.so.preload` and is never `dlopen`ed.
- **`csrc/thead/ppu-slicing-shim/testing/*.c` — never shipped, and may link the SDK freely.** They only ever run
  inside `gpustack/thead-ppu-devel`, so linking `-lhgml -lhggc -lhggcrt1` is the right call: the linker resolves
  the headers' `_v2`/`_v4` macro mappings and type-checks every signature, where a hand-written `dlsym` table
  would just be a second place to get an ABI name wrong. `hgml_nohook.c` is the exception that proves the rule —
  it is a *preloaded* control, so it obeys the shipped-code linkage rule despite living here.
- **One artifact is `.cu` and built by `hgcc`, the vendor's own compiler**, and it is the only one:
  `testing/hggc_launch_load.cu`. Compute throttling cannot be judged without a workload that occupies the card,
  and there is no way to occupy a PPU from plain C — the `<<<>>>` launch it compiles to is what funnels down into
  the interposed `hgLaunchKernel`. It follows the rule above rather than adding a new one: gate-only, never
  shipped, and free to link the SDK.
- **A product source may include an SDK header it must not link.** `hggc/hggc_compute.c` includes `hgml.h` and
  reaches every entry through `dlopen`/`dlsym`, so its function pointers are typed by the header (`__typeof__` of
  the declaration) while `DT_NEEDED` stays `libc.so.6` — case 1 asserts both halves, and the pair is the evidence
  that the utilisation feedback is resolved at runtime rather than linked.

### Implementation Plan

Eighteen tasks in two stages. **Stage 1 (T1–T11, all delivered)** proved the mechanism and landed the two files
that carry it: unblocked at the start were T1, T8, T10; after T1 came T2, T4, T6; after T4, T5 and T7.
**Stage 2 (T12–T18)** builds the library around them, and is a chain rather than a fan: T12 fixes the
injection contract every later task reads, T13 builds the ledger the rest sit on, T14 and T15 add the two quotas,
T16 gives the tree one build entry point before more artifacts land on it, and T17 then runs over disjoint paths
before T18 reads what they produce.

Paths in `Owns:` are relative to the repository root; `SKILL` abbreviates
`.claude/skills/gpustack-operator-xbuild-and-verify`.

Two edges below are labelled *write conflict* rather than *depends on*: they exist only because two tasks would
edit the same file, not because one needs the other's output. They are kept honest so a future re-cut can remove
them by splitting the file rather than by guessing whether the dependency was real.

**The `Owns:` lines below under-declare this file, and that is measurable rather than hypothetical.** Only T10 and
T11 name `specs/2026-08-03-thead-ppu-slicing-shim.md`, but eleven of the thirteen commits on this branch
touched it: every task corrected the spec as it learned something the plan had wrong — the vendor wrapper's real
name, the runtime-versus-driver naming of the Gate 2 experiment, the launch-entry count. An attempt at ship time
to reorder the commits into plan order conflicted here on the first move, which is the honest signal that these
tasks were never disjoint in the way their `Owns:` claimed. A re-cut has two ways out, and picking neither is what
produced this: declare the spec in every task's `Owns:` and accept that they serialise, or route all write-backs
through one spec-reconciliation task per phase so the tasks themselves never touch it. Stage 2 takes the first
way out: T12 and T13 declare every file they touch, this one included, and each is a wide `Owns:` as a result.

- [x] **T1 · Runtime-capability gate in the verify skill** (prefactor)
      Blocked by: None
      Owns: `SKILL/scripts/lib.sh`, `SKILL/scripts/preflight.sh`
      Gate: review
      Acceptance: `lib.sh` resolves `XB_CTR` by probing `docker` then `nerdctl` **on the target** (not the
      caller), honouring an explicit `XB_CTR` override and an `XB_CTR_ARGS` knob for the containerd namespace a
      k3s/rke2 host needs, plus an `XB_SSH_OPTS` knob carrying extra ssh options so a target reachable only
      through a jump host (`-J`) can be driven by the same case scripts rather than by hand outside the skill.
      `preflight.sh` gains no container invocation — it has none today — so its change is
      the gate itself: *run-capable* (any runtime resolves) is the only FAIL, *build-capable* (`docker buildx`)
      becomes a WARN naming "build on a docker host, then load the image here", and three THead hardware WARN
      rows appear (`ppu-smi` probed both by `${PPU_HOME}/ppu-smi/bin` and by `PATH`, the `/dev/alixpu` +
      `/dev/alixpu_ctl` + `/dev/alixpu_ppu*` nodes, the `alixpu` module). No buildkitd is started. `build.sh` and
      the existing Ascend/NVIDIA cases are untouched.
      Verify: `bash -n` on both files; `bash SKILL/scripts/preflight.sh` locally → `FAILS=0`, a build-capable PASS
      row and three THead WARN rows; `XB_CTR=nosuchruntime bash SKILL/scripts/preflight.sh` → exactly one FAIL row
      and `FAILS=1`.

- [x] **T2 · HGML visibility shims + case 1**
      Blocked by: T1
      Owns: `csrc/thead/ppu-slicing-shim/hgml_dlsym_hook.c`, `csrc/thead/ppu-slicing-shim/testing/hgml_nohook.c`, `SKILL/cases/thead-case-1.sh`
      Gate: review
      Acceptance: **two** artifacts, because Gate 1 needs its own negative control and that control must be built
      by the same task that builds the subject. `hgml_dlsym_hook.so` interposes `dlsym` and rewrites
      `hgmlDeviceGetMemoryInfo` and `hgmlDeviceGetMemoryInfo_v2` from an env quota, leaving `_v2`'s
      `version` and `reserved` untouched — passed through from the vendor rather than restored from the
      caller, so a version mismatch the vendor reports stays visible instead of being masked by the
      wrapper; `hgml_nohook.so` defines the same two HGML symbols and does
      **not** interpose `dlsym`. Both include `<stdbool.h>` and `<stddef.h>` before `hgml.h`, which supplies
      neither. Case 1 compiles both inside `gpustack/thead-ppu-devel:2.1.1` through `${XB_CTR}` and asserts, per
      artifact: compiles clean; `DT_NEEDED` is empty or exactly `libc.so.6`; no `GLIBC_` requirement above
      `GLIBC_2.17`; and the intended symbols are **defined and exported** — `GLOBAL DEFAULT` with a non-`UND`
      section via `readelf -W --dyn-syms`, never `nm -D | grep`, which also matches the *imported* `dlsym` and
      would pass for any library that merely calls it. The case records each artifact's staged path and `sha256`
      so T3 consumes exactly what T2 produced. No hardware, no device nodes.
      Verify: `bash SKILL/cases/thead-case-1.sh` on the local docker host → `FAILS=0`, with both artifacts'
      `sha256` printed.

- [x] **T3 · Case 2 — Gate 1, `ppu-smi` visibility**
      Blocked by: T2
      Owns: `SKILL/cases/thead-case-2.sh`
      Gate: review
      Acceptance: a **mechanism group that needs no hardware**, then four visibility rows that do. The
      mechanism group exists because the claim Gate 1 rests on — that `dlsym` interposition works where
      defining the HGML symbols is inert — is decidable without a card: an inline program `dlopen`s
      `libhgml.so` exactly as `ppu-smi` does and reports via `dladdr` which object each memory getter
      resolved in. Preloading the hook must put both getters inside the shim; preloading the control must
      leave both inside `libhgml.so` while its own marker proves it loaded; preloading nothing is the
      reference point. Deferring all of Gate 1 to hardware would have left the spec's central claim untested
      when it did not have to be.
      Then four rows, all preloading **container-scoped only** (`LD_PRELOAD`; the host
      `/etc/ld.so.preload` is never written). A baseline row runs the selected card with no preload and records
      its physical memory figure, so the arms are compared against a measured value rather than the literal
      98304. Arm (a) `hgml_dlsym_hook.so` → that card's memory field equals the configured quota **exactly**;
      loose substring matching is what would otherwise let a wrong value pass. Arm (b) `hgml_nohook.so` → the
      field equals the baseline, **and** the arm independently proves its library actually loaded (a marker the
      shim emits, or `LD_DEBUG=libs` naming the object) — without that proof, a control that silently failed to
      load also shows the physical value and would PASS for the wrong reason, destroying the whole point of the
      control. Arm (c) additionally preloads the vendor `libhggc_wrapper.so` → no recursion, no deadlock,
      timeout-bounded — and it proves the wrapper loaded on the same terms as (b), because an absent one leaves
      the hook working alone at exactly the figure this arm expects; unloadable is a SKIP, never a PASS. `ppu-smi` is invoked by absolute path and every row is decided by **parsed output**, never
      exit status, which is 0 even on `init HGML error: driver is not loaded`. With no hardware every row emits
      `SKIP` with its reason and leaves `fails` untouched.
      Verify: on a PPU host → `FAILS=0` with (a) at the quota and (b) at the baseline; with no hardware → all rows
      `SKIP`, `FAILS=0`.

- [x] **T4 · HGGC driver-layer quota shim**
      Blocked by: T1
      Owns: `csrc/thead/ppu-slicing-shim/hggc_quota.c`
      Gate: review
      Acceptance: a shim distinct from T2's — Gate 2 is about driver-layer *allocation*, which the HGML
      visibility hook cannot enforce or observe. It interposes the **driver-layer** entries behind the paths
      Gate 2 exercises (`hgMemAlloc_v2` for the plain path, `hgMemCreate`/`hgMemMap`,
      `hgMemAllocFromPoolAsync`/`hgMemPoolTrimTo`, `hgMemAllocAsync`, plus the matching free and query entries) —
      not the runtime-layer `hggcMalloc` the workload calls, which lives in `libhggcrt.13.0.so` and whose
      interposition would answer nothing about `libhggc.so`. It
      enforces an env quota — including giving bytes back on free, which means its ledger cannot delete an entry
      by emptying the slot: with open addressing that truncates the probe chain of every key behind it, and
      page-aligned device pointers make that chain the common case rather than the corner one — and emits two
      things Gate 2 cannot work without: a **per-entry call counter**, so
      "the call crossed `libhggc.so`" is decided by counting a call rather than inferred from link or symbol
      evidence; and an explicit **denial marker** on refusal, so a refusal by our quota is distinguishable from a
      failure for any other reason.
      Verify: compiles inside the devel image under the same four linkage assertions as T2, and the counter and
      denial-marker strings are present in the built object.

- [x] **T5 · Case 3 — Gate 2, memory-path completeness**
      Blocked by: T4
      Owns: `SKILL/cases/thead-case-3.sh`, `csrc/thead/ppu-slicing-shim/testing/hggc_mem_paths.c`
      Gate: review
      Acceptance: one row group per memory path — plain (`hggcMalloc` down to `hgMemAlloc_v2`), VMM
      (`hgMemCreate`/`hgMemMap`), pool
      (`hgMemAllocFromPoolAsync` + `hgMemPoolTrimTo`), async — each requiring **three** observations rather than
      one, because any single one of them passes for the wrong reasons: an under-quota allocation succeeds; the
      same over-quota allocation succeeds when the shim is *not* injected (proving the refusal is ours and not the
      platform's); and the over-quota allocation with the shim is refused **carrying T4's denial marker**. The
      async and pool paths synchronise before the row is judged. Whether the call crossed `libhggc.so` is read
      from T4's counter. A branch that reaches `libalippu.so` without crossing `libhggc.so` is recorded as a named
      FAIL row — the driver-layer-only premise breaking is a finding to act on, not something to retry — but a
      group where **no** counter moved is a separate row, because `hgMemAlloc` also exports a v1 symbol with
      different parameter types and "the shim watched the wrong name" must not be reported as the premise
      breaking.
      A fifth row sits outside the four
      paths because none of them can reach it: each runs one allocation in a fresh process, so a shim that refunds
      nothing passes them all and its ledger dies with the process. That row fills the quota with two
      allocations, frees both, and asks for the whole quota again.
      Verify: on a PPU host → `FAILS=0` with four path groups and the refund row present; with no hardware → all
      rows `SKIP`.

- [x] **T6 · Case 4 — Gate 3, utilisation characterisation**
      Blocked by: T1
      Owns: `csrc/thead/ppu-slicing-shim/testing/hgml_util_probe.c`, `SKILL/cases/thead-case-4.sh`
      Gate: review
      Acceptance: this gate needs no shim, so it must actually run whenever hardware is present rather than wait
      on anything else — and its answer is a design input, deciding whether the PID loop's feedback can be
      per-process. The probe calls `hgmlDeviceGetProcessUtilization` under a **controlled load** it starts itself
      and records: whether the call is supported at runtime, and whether the returned `pid` is in the container's
      or the host's PID namespace, established by matching it against a PID the probe knows. A call that succeeds
      but returns an empty sample is **not** a pass — that is the shape a false PASS would take here, and neither
      is one whose samples all belong to other processes: the query asks for all history, so a neighbour's stale
      sample would otherwise stand in for the per-process feedback this gate is about.
      Verify: on a PPU host → a concrete supported/unsupported verdict and a concrete host/container verdict, each
      with its captured output; with no hardware → `SKIP`.

- [x] **T7 · Case 5 — multi-card per-device quota independence**
      Blocked by: T4
      Owns: `SKILL/cases/thead-case-5.sh`
      Gate: review
      Acceptance: two **distinct** cards get different quotas and neither leaks into the other's accounting — one
      index twice satisfies every row while testing nothing. Cards are chosen by being **idle**, never by a fixed index — the host runs production inference and one card already holds
      ~91 GB.
      Verify: on a PPU host → `FAILS=0`; with no hardware, or with fewer than two idle cards → `SKIP` naming which.

- [x] **T8 · Reference docs and troubleshooting**
      Blocked by: None
      Owns: `SKILL/references/thead-hgml-dlsym-and-ppu-smi.md`,
      `SKILL/references/thead-ppu-sdk-and-glibc.md`, `SKILL/references/troubleshooting.md`
      Acceptance: the first records the `dlopen` + explicit-handle mechanism, why defining the HGML symbols alone
      is inert, and why the control arm must prove it loaded. The second records the SDK layout as observed in
      `2.1.1` (the three `bin` directories, `VERSION.txt`'s two lines), the `hgml.h` zero-includes constraint, and
      the `GLIBC_2.17` floor. `troubleshooting.md` gains a THead section whose first entry is `ppu-smi` exiting 0
      on failure. Every claim traces to this spec or the research report.
      Verify: both files exist and follow the existing references' heading shape; `grep -n 'exit' 
      SKILL/references/troubleshooting.md` finds the `ppu-smi` entry.

- [x] **T9 · `SKILL.md` wiring**
      Blocked by: T2, T3, T5, T6, T7
      Owns: `SKILL/SKILL.md`
      Acceptance: a third Cases table whose five titles match each script's banner verbatim; a THead env-knob
      group covering `XB_CTR`, `XB_CTR_ARGS` and the quota variables; and one explicit
      `Bash(bash …/cases/thead-case-N.sh*)` entry per case in `allowed-tools`, plus an entry for any container
      command the skill instructs Claude to run directly — the list has no glob, and today only
      `docker buildx|images|info` are allowed, so a bare `nerdctl` would be denied. Not blocked by T8: the table
      needs the case banners, not the reference prose.
      Verify: each of the five titles `grep`s in its own script; `allowed-tools` contains five `thead-case`
      entries.

- [x] **T10 · F3 symbol manifest**
      Blocked by: None
      Owns: `SKILL/references/thead-hggc-symbol-manifest.md`, `specs/2026-08-03-thead-ppu-slicing-shim.md`
      Acceptance: the F3 table's symbol claims are re-established against the **`2.1.1` libraries in the devel
      image** rather than the host's copies, and recorded as a checked-in manifest — the exact symbol names, the
      image digest they came from, and the one-liner that regenerates them — because an aggregate count (437 base
      names / 620 exported / 183 suffixed / 23 launch entries) cannot be re-checked by anyone later. Any count
      that differs is corrected in F3, which then states which SDK version its numbers describe.
      Verify: re-running the recorded command reproduces the manifest byte for byte.

- [x] **T11 · Execute the three gates and record the results**
      Blocked by: T3, T5, T6, T7; **an available PPU host** (external precondition — satisfied: a 16-card
      `PPU-ZW810E` host); T10 (write conflict on the spec file, not a functional dependency)
      Owns: `specs/2026-08-03-thead-ppu-slicing-shim.md`
      Gate: review
      Acceptance: F2 gains a PASS/FAIL row per gate with captured output. A FAIL is recorded as a finding together
      with its consequence for the design: Gate 2 failing escalates interception to the UMD layer; Gate 3 failing
      falls back to card-total feedback with the controller-coupling risk written down.
      Verify: three gate rows present, each carrying the command that produced it and that command's output.

- [x] **T12 · Per-card quota keying and the `HGGC_` env contract** (Stage 2 prefactor)
      Blocked by: None
      Owns: `csrc/thead/ppu-slicing-shim/hggc_quota.c`, `csrc/thead/ppu-slicing-shim/hgml_dlsym_hook.c`,
      `SKILL/cases/thead-case-{1,2,3,5}.sh`, `SKILL/SKILL.md`, `SKILL/references/troubleshooting.md`
      Gate: review
      Acceptance: the contract F5 fixes, in place before anything reads it — every later task depends on which
      variable names exist and on what an absent one means. `VPPU_DEVICE_MEMORY_LIMIT_MIB` becomes
      `HGGC_DEVICE_MEMORY_LIMIT_<i>`, and `HGGC_DEVICE_SM_LIMIT` is introduced even though nothing enforces it
      until T15 — so that T15 does not also change the contract. **This is not a rename:** `hgMemAlloc` takes no
      device argument, it charges the current context's device, so the shim must resolve that device
      (`hgCtxGetDevice`, or `hggcGetDevice` on the runtime side) before it can pick a figure. `<i>` is the
      container-local index, because the SDK renumbers devices inside the container. A sliced container missing the
      variable for a card it holds is an init error, never transparent — and the compute variable must not inherit
      `SlicedCoresPercent`'s default of 100, which is what would make a 25% request silently mean a whole card.
      `hgml_dlsym_hook` reports the figure for the device the **caller asked about**, not one figure for every card.
      `LIBHGGC_LOG_LEVEL` lands in the same task, because it changes what the cases can see: the shim prints
      unconditionally today, so introducing a quiet default without the level the cases pin would silence four of
      the five. Denials stay at the default level; the load markers and the counter dump move to `2`, and the cases
      set it.
      Owns is deliberately wide: every case script carries the env name, and under-declaring this is the mistake
      recorded above the plan.
      Verify: `bash SKILL/cases/thead-case-1.sh` asserts the new strings and that no `VPPU_` env name survives;
      cases 2/3/5 still `FAILS=0` on the PPU host with the renamed variables; and a run at the default level shows
      a denial but neither the load marker nor the counter dump, which is what proves the level is real rather than
      a variable nothing reads.

- [x] **T13 · `common/`: the ledger, the lock, and the usage region**
      Blocked by: T12
      Owns: `csrc/thead/ppu-slicing-shim/common/**`, `SKILL/cases/thead-case-6.sh`,
      `csrc/thead/ppu-slicing-shim/hggc_quota.c`, `csrc/thead/ppu-slicing-shim/hgml_dlsym_hook.c`,
      `csrc/thead/ppu-slicing-shim/testing/hggc_mem_paths.c`, `SKILL/cases/thead-case-1.sh`, `SKILL/SKILL.md`,
      `SKILL/references/troubleshooting.md`
      Gate: review
      Acceptance: the file-mapped cross-process region F5 specifies — a magic, a layout version, per-card slots and
      per-process slots — guarded by a process-shared lock **held until the real allocation returns**, which is
      what closes the check-then-alloc race the two Stage 1 files still have. The quota is re-read on every call
      rather than written once by whichever process created the region: that trap is HAMi-core's, where a stale
      cache freezes the old limit until the file is deleted. A missing or unparsable quota is an init error, never
      "no limit" — the trap on flexai's side. No `hg*`/`hggc*`/`hgml*` type may appear in this directory, which is
      what makes the charge/refund arithmetic and quota parsing unit-testable without a device. The two existing
      files move onto this ledger and drop their process-local accounting.
      Five things this task settled that the plan left open, all recorded in F5 and Code Style:
      the lock is an `fcntl` record lock per card (a `pthread_mutex`/`sem_t` in the region would need `-lpthread`
      and break the `libc.so.6`-only guarantee); the bytes are charged BEFORE the vendor call and refunded if it
      fails, so no window exists in which memory is held unaccounted; a dead process's charge is swept by liveness
      on the path that would otherwise refuse, or a killed workload would hold the quota down for the life of the
      region file; and the key → (card, bytes) map stays PROCESS-LOCAL, because a device pointer is a value in one
      process's address space and a shared table keyed by it would let one process's free refund another's
      allocation. The shared thing is the total, which is what the region carries. And the process-local state is
      reset on first use in a new process, because `fork()` duplicates a held spinlock and the thread-local lock
      depth behind it, which a child would respectively wait on forever and mistake for its own re-entry —
      `pthread_atfork` being unavailable for the same `-lpthread` reason.
      "An init error" is a load-time report plus a latch that refuses every allocation, not `_exit()`: this library
      arrives through `/etc/ld.so.preload`, so exiting would kill every process in the container including the
      shell a human would diagnose it from.
      Verify: unit tests for charge, refund, the tombstone case, quota parsing, the region layout read back by
      documented offset, an unknown layout version refused, two forked processes serialised on one lock, and a dead
      process's charge reclaimed — **built and run by case 6 with no device and no SDK header**, not by `make test`,
      which runs `go test` only; and case 6's hardware half — **two processes in one container against one quota** —
      where the second must NOT be granted memory the first already holds, which is exactly what a process-local
      ledger does.

- [x] **T14 · `hggc/`: the memory quota over the full driver-layer surface**
      Blocked by: T13
      Owns: `csrc/thead/ppu-slicing-shim/hggc/**` (memory entries), `csrc/thead/ppu-slicing-shim/hggc_quota.c`
      (deleted — it becomes the module), `csrc/thead/ppu-slicing-shim/testing/hggc_mem_paths.c`,
      `SKILL/cases/thead-case-{1,3}.sh`, `SKILL/references/troubleshooting.md`
      Acceptance: `hggc_quota.c`'s eleven entries become this module, backed by `common/`'s ledger, and coverage
      extends to every allocation, free, query and pool symbol plus **all** suffix variants — 620 exported `hg*`,
      183 suffixed, 437 base names in `2.1.1` per the checked-in manifest. `hgGetProcAddress`,
      `hgGetProcAddress_v2` and `hgGetExportTable` are covered too, because a caller that resolves an entry point
      through one of those walks straight past any interposition of the entry point itself. The plain v1 names get
      their own prototypes rather than a reuse of the `_v2` ones, since they take different parameter types.
      Owns is wider than the plan's: case 1 stages and compiles the module, so a new directory changes it, and the
      exerciser needs a path that resolves through `hgGetProcAddress` before case 3 can read one.
      Four things this task settled, all recorded in F3: the memory surface is **35** exported names and the module
      is 38 with the resolvers, split across four translation units behind a private `VPPU_INTERNAL` seam rather
      than one file; what is charged and what is only counted (host memory, address mapping and the pool entries
      are counted, because their bytes are taken by an entry that *is* charged); the pitched entries admit on the
      caller's width and reconcile the charge to the driver's stride under the same lock, reporting rather than
      refusing a stride that overruns; and this driver's `hgGetProcAddress` already returns the interposed address,
      which makes the substitution a guard for a future SDK rather than the mechanism in force today — printed, not
      inferred, because a guard that matched nothing would look identical.
      Verify (**met**): case 3's four paths and its refund row still `FAILS=0` with the shared ledger behind them
      — plus a fifth `procaddr` path judged on the same three observations, whose over-quota row reads
      `DENIED hgMemAlloc_v2 device=0 request=8589934592 … quota=4294967296` and whose counter shows
      `hgGetProcAddress_v2=1`. Case 1 rose to 38 exported-symbol rows plus a header-checked row for the v1
      prototypes; cases 2, 5 and 6 stay `FAILS=0` on the PPU host with the module behind them.
      What the runtime evidence does **not** cover, stated rather than implied: it exercises four allocation
      *shapes* plus the resolver, not one path per name. The remaining entries — the pitched pair, managed memory,
      the `_ptsz` variants, the v1 forms, host memory and the pool calls — rest on a signature the header
      type-checks and on case 1's assertion that each name is exported. Two mutations were run to keep those from
      being assumed: dropping one name from the ABI table fails the build on the `_Static_assert` that pins it
      against the enum, and retyping one v1 size fails the header-checked row.

- [x] **T15 · `hggc/`: compute throttling, the PID loop**
      Blocked by: T13
      Owns: `csrc/thead/ppu-slicing-shim/hggc/**` (launch entries and the controller),
      `csrc/thead/ppu-slicing-shim/common/vppu_pid.{h,c}` (the controller's arithmetic),
      `csrc/thead/ppu-slicing-shim/common/vppu_{quota,ledger,test}.{h,c}` (the compute figure in the usable latch,
      the controller's knobs, the region's controller words, their tests),
      `csrc/thead/ppu-slicing-shim/testing/hggc_launch_load.cu`,
      `SKILL/cases/thead-case-{1,2,3,5,6,7}.sh`, `SKILL.md`, `SKILL/references/troubleshooting.md`
      Gate: review
      Acceptance: the dimension nothing enforces today. All **16** launch entries are interposed, including
      `hgGraphLaunch(_ptsz)` and `hgLaunchCooperativeKernel(_ptsz)`. The controller is a PID outer loop whose
      feedback input is **this container's per-process** utilisation from `hgmlDeviceGetProcessUtilization` — Gate
      3 established that it is supported, non-empty under load, and reports the container's own pid, which is what
      retires the card-total fallback and its controller coupling. Three departures from the reference are
      required rather than optional: fail-closed (a missing quota or an unresolvable device index is an error, not
      a fixed sleep), a cold-start feed-forward floor derived from the quota so a burst cannot take the whole card
      before the first sample arrives, and gains that are env-tunable and **not** inherited — flexai's are two
      hardcoded triples fitted to their own NVIDIA hardware. The loop's state goes into `common/`'s region, or it
      cannot be tuned on hardware nobody has profiled. Graph accounting is a launch-time coefficient defaulting to
      **off**, with graph and non-graph utilisation logged separately; instantiate-time node accounting is an
      escalation taken only if measurement shows graphs escaping. This task also flips the compute figure from
      reported to refused: T13 latches "unusable" on the memory figure alone, because until this controller exists
      denying every allocation over an unimplemented dimension would fail closed on the wrong thing.
      Owns is wider than the plan's, and for three reasons rather than one: the loop's state lives in `common/`'s
      region, so the region's reserved words become a typed field there; the decision half of the loop went to
      `common/` so it could be unit-tested at all; and the flip means every case that injects the shim has to inject
      the compute figure or be refused.
      Five things this task settled, all recorded in F3: the actuator is a **duty-cycle window** rather than a token
      bucket, because that makes the cold-start floor exactly `window × limit%` instead of a guess at a
      launches-per-period ceiling; the loop steps at a **second** cadence separate from the window, because the
      driver's utilisation figure is slew-rate limited to ~10 points per 100 ms and a loop stepping per window
      oscillates across the whole range (observed, then fixed); the feedback is filtered to this container through
      the ledger's process table rather than by pid namespace; the graph coefficient tightens the window a graph
      launch is admitted in, and the "separately logged" utilisation is per-interval averages, because the driver
      reports one figure per process; and host callbacks are counted but never gated, since delaying a CPU callback
      frees none of the card.
      Verify (**met**): case 7 on a PPU host, `FAILS=0` on all six rows — uncapped 94% mean `smUtil` at 415
      launches/s, capped at 25 it settles at **25%** and 103 launches/s, `hgLaunchKernel=2061` proves the crossing,
      the loop state reads back from the region by documented offset (`limit=25 util=20 allow=19880000`), two
      containers on one card hold 25% each, and a container with no compute figure is refused with the `DENIED`
      marker. Cases 1, 2, 3, 5 and 6 stay `FAILS=0` with the compute figure injected.
      What the runtime evidence does **not** cover, stated rather than implied: it exercises one launch entry —
      `hgLaunchKernel`, which is what the runtime's `<<<>>>` funnels into — not sixteen. The other fifteen rest on
      the header type-checking each signature and on case 1's assertion that each name is exported, the same footing
      as T14's uncalled memory entries. Two mutations kept the case itself honest: removing the wait made the capped
      row fail at 94%, and feeding the loop the card total made the two-container row fail at 11%/14% — the second
      only after its band was tightened, since the first version of that row passed the very defect it exists for.

- [x] **T16 · Prefactor: one build entry point for the shim tree, and a README**
      Blocked by: T15
      Owns: `csrc/thead/ppu-slicing-shim/build.sh`, `csrc/thead/ppu-slicing-shim/README.md`,
      `csrc/thead/ppu-slicing-shim/testing/dlsym_origin.c`, `SKILL/scripts/build.sh`,
      `SKILL/cases/thead-case-{1..7}.sh`, `SKILL.md`
      Gate: review
      Acceptance: the THead backend gets the shape the other two already have — `build.sh <target>` produces the
      artifacts, the cases inspect and run them. Today the verification scripts hold the product's build recipe:
      case 1 carries the translation-unit lists, the include roots and the flags, and each of five more cases
      carries a compile line for its own gate binary. `hggc_mem_paths` is compiled by three of them and two had
      already drifted onto different source paths, which is the concrete symptom rather than an aesthetic
      complaint. The recipes move into the tree that owns the sources (`build.sh lib|test|unit|check v1|list`),
      the skill's `scripts/build.sh` gains an `xbuild-thead-ppu` arm that stages the tree and calls it inside the
      SDK image, and `testing/dlsym_origin.c` stops being 28 lines of C inside a shell heredoc. What stays in the
      cases is what a case is for: the assertions. A `README.md` covers building and using the shims for a reader
      who never opens the skill.
      This crosses a Non-Goal deliberately and it is recorded above: F4 excluded the `scripts/build.sh` arm as
      build-system work. The scope it excluded was the `pack/` wiring, which is still handed off; a *verification*
      entry point that compiles the checked-in sources is what the other two backends already have.
      Ordered before T17 rather than after T18 on purpose: T17 adds a case arm and T18 a new artifact, so both
      would otherwise be written in the shape this task removes.
      Verify (**met**): `build.sh xbuild-thead-ppu` on a PPU host stages 23 sources and compiles all eight
      artifacts in one pass, and cases 1–7 then report `FAILS=0` against exactly what it produced;
      `./build.sh unit && ./vppu_test` runs on a macOS development machine with no SDK and no device;
      `shellcheck -x -S warning` is clean on all nine scripts this task wrote or rewrote (and on the two it did
      not touch), leaving only two suite-wide `info` idioms below the gate — `SC1091` for a sourced `lib.sh`
      shellcheck resolves only from the script's own directory, and `SC2015` for the `A && B || C` verdict line
      every case in all three backends ends with; and
      `grep -c 'xput\|SRC_DIR\|gcc -\|hgcc -'` over the seven case scripts is **0** — 115 lines of build and
      staging code left them, and case 1 now records each artifact's translation units from the build itself
      instead of a copy it kept.
      One thing the conversion got wrong and the run caught: cutting case 5's build block took its `skip_all()`
      definition and its card-list split with it, and the case died on `$1: unbound variable` before printing a
      single row. The outer `FAILS=0` grep reported FAIL correctly, which is the property that made it visible —
      a case that exits early cannot pass.

- [x] **T17 · `hgml/`: per-card visibility and the re-entrancy guard**
      Blocked by: T12, T13
      Owns: `csrc/thead/ppu-slicing-shim/hgml/**`, `csrc/thead/ppu-slicing-shim/testing/dlsym_stack.c`,
      `csrc/thead/ppu-slicing-shim/build.sh`, `csrc/thead/ppu-slicing-shim/README.md`,
      `SKILL/cases/thead-case-{1,2}.sh`, `SKILL/scripts/build.sh`, `SKILL.md`
      Acceptance: both public memory getters are covered separately, because their shared helper is `FUNC LOCAL`
      and cannot be interposed. A re-entrancy/origin guard is added: Gate 1 arm (c) exercised **one** vendor
      wrapper in one ordering, which is not the same as proving no ordering recurses. `used` comes from
      `common/`'s ledger rather than being handed back from the vendor unchanged, so the figure `ppu-smi` shows
      and the figure the quota enforces are the same number. Per-card, following T12's contract.
      Verify (**met**): all seven cases report `FAILS=0` after the move, with case 2 carrying three new groups —
      the stacked-interposer mechanism rows in both preload orders, arms (d)/(e) at call time, and arm (f) for the
      `used` side. Two mutations kept them honest: handing back the vendor's `used` makes arm (f) fail at
      **2081MiB** against the ledger's 2048 (the driver counts context the quota never charged for, which is
      exactly the two-numbers problem), and **removing the re-entrancy guard changes no row at all** — recorded in
      F3 as a measured limit of the verification rather than left to look covered.
      The task also settled the two questions T14–T16 deliberately left: `hgml_dlsym_hook.c` moves out of the tree
      root into `hgml/`, so the root holds no product source; and the tree keeps **two** shared objects, one per
      interposed library, which the README now states as a decision instead of leaving "the enforcement half of
      libvppu.so" to imply one.

- [x] **T18 · `tools/`: the reader, and the region documented as a contract**
      Blocked by: T13, T15
      Owns: `csrc/thead/ppu-slicing-shim/tools/**`, `SKILL/references/thead-usage-region.md`, and — declared as
      T12–T17 did — `csrc/thead/ppu-slicing-shim/build.sh` (a `tool` verb and the `TOOLS` list),
      `csrc/thead/ppu-slicing-shim/README.md`, the skill's `scripts/build.sh` (stage + compile),
      `cases/thead-case-1.sh`, `cases/thead-case-6.sh`, `SKILL.md` and this spec.
      Acceptance: a reader that prints, for every card the container holds, both dimensions' quota and usage —
      including the compute **limit**, which appears in no `ppu-smi` field and which without this tool can only be
      inferred from an init log or a stress test. It reads `common/`'s region rather than any shim symbol, so the
      same path serves the future scraper; the Ascend precedent is `enpu-monitor`, mounted into the container for
      the same reason. The region's magic, version and field layout are written down as a contract, because a
      scraper written against an undocumented mmap of a C struct breaks on the next field added.
      Verify: run it inside a sliced container and match its figures against the shim's own `DENIED` and counter
      output; and parse the region with a small independent reader that uses only the documented layout, never a
      header from `common/`.
      **Met.** `tools/ppu_monitor.c` → `ppu-monitor`, and case 6 Part B now reads one held quota **three ways and
      requires the three to agree**: the shim's own `DENIED hgMemAlloc_v2 … accounted=4294967296 quota=4294967296`,
      the reader run with `LD_PRELOAD` cleared, and `od` at offsets 96 and 112. Case 1 carries the contract's own
      rows — a region written by `dd` **from the reference document alone**, read back including the 576-byte
      stride and the process slot, plus a bumped layout version refused (exit 2) and an absent region reported as
      its own outcome (exit 1). All seven cases: `FAILS=0`, card 0 untouched.
      Three decisions carry the task. **It links none of `common/`'s ledger code**: `vppu_ledger_used()` maps the
      region lazily, so calling it would *create* one — a reader that conjured a slice into existence for anything
      that looked at the container — and the rest of that file takes the card's lock, which would let a monitor
      block behind a hung vendor allocation. So the tool takes the struct and its `_Static_assert`s from the header
      and does its own read-only `mmap`; being a second parser of the layout is the point of having a contract.
      **It gets no SDK include path and no vendor library**, which is what makes it runnable in a container with no
      device — asserted as `DT_NEEDED: libc.so.6` rather than trusted from the recipe. And **it does not derive the
      throttle as a percentage**: `allow_ns` is in the region but the window it is a fraction of is not, since the
      period is the container's own `HGGC_SM_CONTROL_PERIOD_MS`, so a reader that could not see that environment
      would print a confident wrong number. `allow_us` is reported raw.
      Two limits are recorded rather than papered over: a card the container holds but has **never allocated on has
      no row**, because the region records a card the first time an admission touches it; and the region shows what
      was charged, so a figure can be one allocation stale by design (the read side takes no lock).
      Three mutations, each failing exactly one row and nothing else: accepting any layout version fails only the
      refusal row; reporting the quota where the charge belongs fails only the documented-offsets row; and inserting
      a word before the controller in `vppu_ledger.h` fails the reader's **build** on four `_Static_assert`s, which
      is the contract biting in the one place a second consumer makes it real. The `od` cross-check earned itself
      on the first run: it parsed the holder's interleaved `[vppu]` log lines as numbers, so those lines are now
      tagged rather than fenced between section markers.

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

None. This spec's deltas are shell scripts (the case scripts and the shim tree's own `build.sh`), a C source tree
under `csrc/` and spec text. The repository wires no shell
linter and no shell test harness — `hack/lint.sh` covers Go (`golangci-lint` + `goimports`) and the helm chart
only — so `bash -n`, a by-hand `shellcheck -x -S warning`, and each case script's own `FAILS=` contract are the
gate for the shell, and the C is gated by case 1's compile-and-linkage assertions. `make lint` and `make test` are
run unchanged, as evidence that no Go package moved.

#### Unit tests

**Stage 1 added none**, and could not: it adds no Go code, and the C it added has no unit-test harness to join.
The three files under `.../testing/` are gate scaffolding whose contract is the gate they serve, and the two
product files' contract is case 1's compile-and-linkage assertions plus the mechanism rows in case 2.

**Stage 2 introduces the first unit-testable surface in this library, and that is one of the reasons `common/`
exists as a directory.** The rule that no `hg*`/`hggc*`/`hgml*` type may appear there is what makes it testable
without a device:

- `common/` ledger arithmetic — charge, refund, refund of an unrecorded key, table-full behaviour, and the
  tombstone case Stage 1 got wrong (two colliding keys, the first freed, the second's refund must still land).
  The pre-fix reproduction under F2 is the regression this must keep closed.
- `common/` quota parsing — a percentage-derived MiB figure, an absolute figure, a malformed value and an
  out-of-range value, each of which must be an error rather than a silent zero.
- `common/` region layout — a round trip through the documented magic and version, plus a reader at an older
  layout version refusing rather than misparsing, since T18 makes that layout a contract.
- `common/` compute-controller arithmetic (**added by T15**, and the reason its decision half was put here rather
  than in `hggc/`) — the cold-start floor is exactly the quota's share of the window and the first step ignores its
  measurement; a card pinned however little it is given squeezes the window to its floor and never closes it; the
  integral stays clamped while it sits there; a workload that cannot reach its cap keeps the whole window without
  oscillating; and the loop converges within a few points of three different caps against a simulated card whose
  utilisation overruns the window it was given. The last one is the only place convergence can be asserted at all —
  hardware can show that it settled, not that it settles.
- `common/` bounded-knob parsing (**added by T15**) — zero is a legitimate value for a gain where it is not for a
  quota, so this is a second parse rather than a reuse, and each knob falls back to its default on a malformed or
  out-of-range value instead of refusing the container.

The C tests need a harness this repo does not have (`hack/test.sh` runs `go test` only). Adding one was part of
T13 and stayed as small as the job needed: `common/vppu_test.c` is a single `main()` printing the same
`STATUS | CHECK | DETAIL` rows every case script prints, and case 6 compiles and runs it. No framework, and no
change to `hack/`. **Delivered by T13**, covering the three groups above plus two the plan did not name — two
forked processes serialised on one card's lock (the check-then-allocate race, with a 150 ms window so a scheduler
cannot miss it), and a dead process's charge reclaimed.

Two conventions this first C test suite establishes, both learned the hard way:

- **The region tests fork; the parent never maps one.** The mapping is latched per process on purpose, so a parent
  that mapped once could not then test a second region — and the cross-process assertions need separate processes
  anyway.
- **Never call something that writes an out-parameter inside an assertion whose detail also formats that
  out-parameter.** Argument evaluation order is unspecified and both orders were observed here: clang evaluated the
  condition first, the devel image's gcc the varargs first, so three rows PASSed while printing figures that never
  existed (`device=-1 bytes=0` for a record stored as `device=2 bytes=4096`). The assertions were right and the
  evidence was fiction, which is worse than a failure — a FAIL row would have printed the same fiction. The mutating
  call goes in its own statement.

Inherited by the handed-off operator work, each with the existing file it should mirror — per-vendor tests are an
established pattern here, so there is no new convention to invent:

- `pkg/devicemanager/detector/thead`: `2026-08-03` - `0%` (no test file today); mirror
  `pkg/devicemanager/detector/ascend/device_test.go`. Must cover that `Status.LogicalSliced` stays the zero value
  when the operator image ships no built THead library — the guard Ascend implements at
  `detector/ascend/device.go:497-524` — and is non-zero when it does; and that
  `device.SetGroupSlicedDetails(grpList)` is called, since omitting it leaves the group aggregate empty and
  `IsLogicallySliceable()` (`api/worker/v1alpha1/instance_type.go:209-211`) then reports the pool as
  un-sliceable with nothing failing loudly.
- `pkg/devicemanager/allocator/thead`: `2026-08-03` - `0%` (no test file today); mirror
  `pkg/devicemanager/allocator/nvidia/deviceplugin_test.go`. Must cover that the `Sliced` server is registered
  only when `!opts.NoSliced`; that the allocate response carries the three added mounts (`/etc/ld.so.preload` ro,
  the library ro, the lock/ledger directory rw) alongside the existing `/dev/alixpu`, `/dev/alixpu_ctl` and
  per-card `/dev/alixpu_ppu<N>` devices; and that a missing quota configuration is an error rather than a silent
  "no limit".
- `pkg/device`: already covered by `pkg/device/sliced_test.go`; no change expected.

#### Integration tests

The case scripts are this spec's integration layer; each is its own task's `Verify`:

- `scripts/build.sh xbuild-thead-ppu` — hardware-free, and the step every case below now depends on: it stages the
  shim tree onto the target and compiles every artifact inside the SDK image by calling the tree's own `build.sh`
  (ten after T18 added the reader). **As built (T16) this is where the compile recipes live**; a case
  that carried one could drift from what ships, and three of them had.
- `thead-case-1.sh` — hardware-free, runs on any amd64 container host: both shims compile inside
  `gpustack/thead-ppu-devel:2.1.1`, `DT_NEEDED` is empty or `libc.so.6`, no `GLIBC_` above 2.17, and the
  interposed symbols are `GLOBAL DEFAULT` and not `UND` while the modules' own internals are not. This is where
  coverage is machine-checked rather than claimed: T14 made it assert all 38 of the quota module's memory-surface
  names and added a syntax-only compile that has `hggc.h` itself check the five v1 prototypes the header's mapping
  otherwise hides; T15 took it to **54** with the launch entries, and added the string/`DT_NEEDED` pair that says
  the compute loop resolves HGML at runtime rather than linking it. T18 added the region contract's own rows: the
  case writes a usage region with `dd` **from the documented offsets** — never from a header in the tree — and the
  reader must read those figures back, refuse a bumped layout version, and report an absent region as its own
  outcome rather than as a corrupt one.
- `thead-case-2.sh` — Gate 1: baseline, hook arm, load-proving negative control, and a vendor-wrapper arm that
  proves the wrapper loaded too, since an absent one leaves the hook working alone at the right figure. T17 added
  three groups: a second `dlsym` interposer stacked in **both** preload orders (hardware-free, and where the
  ordering constraint was measured), the same pair at call time, and the `used` side — one process spends half the
  quota and holds it while `ppu-smi` in the same container must report that figure out of the shared ledger.
- `thead-case-3.sh` — Gate 2: **five** memory paths × (under-quota success, uninjected success, marked denial, a
  moved counter), plus a row for the refund path, which the five cannot reach because each allocates once in a
  fresh process. T14 added the fifth path, `procaddr`: the other four reach an allocation entry by name, so the
  linker resolves it and a preloaded definition wins — that one asks `hgGetProcAddress` for the driver's own
  address and allocates through the returned pointer, which is how the runtime layer binds what it needs.
- `thead-case-4.sh` — Gate 3: supported-at-runtime — by a sample carrying the probe's **own** pid, the query
  returning all history — and host-vs-container PID under a controlled load.
- `thead-case-5.sh` — two idle and **distinct** cards, independent quotas.
- `scripts/preflight.sh` — verified in both directions: a real runtime gives `FAILS=0` with a build-capable WARN,
  and `XB_CTR=nosuchruntime` gives exactly one FAIL.

Stage 2 adds two, and each covers a property no existing case can reach:

- `thead-case-6.sh` — **two processes in one container against one quota**, and separately **one container holding
  two cards with different quotas**. Both are gaps rather than refinements: Stage 1's ledger is process-local, so
  the first case fails against it by construction, and Stage 1's env is container-wide, so the second cannot even
  be expressed. Case 5's multi-card evidence is two containers with one card each, which is a different claim.
  **As built, the second half is Part C**, and it arrived last: the task list delivered Part B and left Part C
  claimed-but-absent until the ship-time spec review caught it. It asks for a size BETWEEN the two figures on
  each index — refused on the smaller card naming that card's own quota, served on the larger — because one
  container-wide figure cannot produce both answers, and it is the only row in the suite that can say so. Two more
  rows arrived with the cross-model review's first finding: a VMM allocation whose `prop` names the *other* card,
  and an allocation from the other card's default pool, both issued from a context on the smaller card — each
  succeeds only because it is charged to the card it names, and each was verified to fail against the pre-fix code.
  T18 made it the cross-check for the reader as well: while the whole quota is held, the same figures are read
  **three ways and required to agree** — the shim's own `DENIED` line, `tools/ppu-monitor` with `LD_PRELOAD`
  cleared, and `od` at the documented offsets. The third is what makes it a test of the contract rather than of the
  struct, and it already earned itself: the first run parsed the holder's interleaved `[vppu]` log lines as
  numbers, so the `od` output is tagged rather than fenced between section markers. A last row asks the same keying
  question of the **visibility** shim: `ppu-smi` inside that two-card container must report each card its own total.
  Case 2 already proves it reports a quota, but with one card a shim answering card 0's figure for every HGML handle
  would pass it — two cards is the only shape that tests the handle-to-index step.
- `thead-case-7.sh` — compute throttling: a compute figure below 100 under saturating load, judged on the
  container's own `smUtil` rather than the card's, with a second container on the same card keeping its share and
  the PID loop's state read back from the region. **As built (T15) it carries six rows, plus two added at ship
  time for the cap being per card** — one container holding two cards at 50% and 25%, each load judged against its
  own band with both loads running at once, and the two figures read out of the region at offsets 112 and 688 so a
  single number copied into every slot cannot pass. Those two inject the indexed figures **without** the un-indexed
  one, which is what makes the row fail loudly against a shim that ignores the index: it then finds no figure at
  all and refuses the container. Of the original six, two exist
  because of what the other four cannot see: the uncapped control runs with the limit at `100` rather than with no
  shim at all, so the cap is the only difference between the two runs; and a container with no compute figure must
  be refused, which is the T13→T15 flip. Both utilisation bands are bounded on **both** sides — a container
  squeezed far below its cap is starved rather than capped, and that is not a hypothetical: the row originally
  accepted anything non-zero and passed a mutation that fed the loop the card total (two containers at 25% settled
  near 13% each).

#### e2e tests

Real-hardware e2e **is** F2's gates, run through cases 2–7 on a PPU host, and it has **run: all PASS** (results
and captured output under F2; the compute gate's evidence sits with T15). Three properties were built in so that a deferred or hardware-less run
could never read as success, and they earned their keep — the no-hardware `SKIP` path had hidden three defects in
the visibility and quota rows that only the real run exposed:

- with no hardware every hardware row emits `SKIP` with its reason and leaves `fails` untouched — never `PASS`;
- no row is decided by `ppu-smi`'s exit status, which is 0 even on `init HGML error: driver is not loaded`;
- the Gate 1 negative control must prove its own library loaded, and Gate 2 must observe a denial marker rather
  than any allocation error — both are cases where a check that silently did nothing would otherwise report
  success.

Those three turned out to be necessary and not sufficient. A cross-model review of the branch found three more
false-PASS paths of exactly the same shape — an arm that never proved the vendor wrapper loaded, a utilisation
verdict that accepted another process's sample, a two-card case that accepted one card twice — and one product
defect (the ledger's refund) that all four PASSing path groups were structurally unable to observe. The lesson the
spec keeps is narrower than "add the three properties": **a row must prove the thing it depends on actually
happened, and a group of rows that each run one operation in a fresh process cannot see state that accumulates.**

No Kubernetes-level e2e is added: this spec changes no operator behaviour. The detector and allocator deltas that
would need it are handed off with the `pack/` wiring, together with the Go unit tests listed above.

## Alternatives

- **Use flexai `xpu-pool-service/direct` as the template instead of HAMi-core.** Its `common/` + `<vendor>/`
  split is genuinely better factored and it is a fifth the size, but it does not hook the management library at
  all, so its slices are invisible to the vendor SMI tool by design — its documented answer is a separate CLI.
  Its injection also replaces the host driver library and re-replaces it on a timer, which is an architectural
  change to GPUStack's deployment model. Its Ascend tree turned out to be prebuilt binaries that reuse none of
  `common/`, so it is not the multi-vendor precedent it appears to be. Its `common/` layering and its
  hold-the-lock-through-allocation semantics are borrowed anyway. F5 added one more reason: its accounting is
  stateless — every allocation asks the vendor library afresh — so there is no ledger and therefore **nothing a
  metrics scraper could read**. Its CLI is not a presentation layer over a usage surface; it is the only surface
  there is.
- **Shadow `libhgml.so` (SONAME shadowing) instead of hooking `dlsym`.** `ppu-smi` loads by bare soname through
  the search path, so a replacement earlier in the search order would be picked up, and this route needs no
  `dlsym` hook and is immune to the vendor-wrapper recursion. Rejected as less extensible: it pins the design to
  shipping a whole replacement management library and to tracking host/SDK version drift, where the host and SDK
  copies of `libhgml.so` already differ.
- **Intercept the runtime layer (`libhggcrt`).** Rejected: the workload brings its own SDK and the runtime's
  `hggcDeviceProp` layout changed across RT generations, so runtime-layer interception is version-fragile
  exactly where we have the least control.
- **Intercept the UKI layer (`ppuGetDeviceRuntimeInfo`) for visibility.** Broadest coverage and it would catch
  anything bypassing HGML, but UKI publishes no header, so it means reverse-engineering an undocumented struct —
  the same class of risk that made the Ascend backend leave several `npu-smi` fields card-wide. Kept as a later
  fallback, not the primary route.
- **HAMi-core's token bucket for compute.** Rejected: its baseline is a fitted hardware heuristic with no
  ground truth on PPU, its per-launch cost ignores block dimensions, and it disables itself silently when host
  PID inference fails.
- **Fetch the SDK in the builder stage on every build.** Rejected: 1.56 GB per build against a presigned URL,
  versus a versioned base image built once per SDK release.
- **Support v2 and v3 now.** Rejected for this iteration: no v2 artifact exists, so the code would be
  unverifiable. Consistent with how the MThreads/Hygon slicing work handled absent hardware.

## Open Questions

- Can one driver-layer product serve both RT generations? Needs a v2 SDK for a symbol-surface diff. Until then
  the artifact directory stays `thead/ppu` without a generation suffix.
- Should the SDK archive be mirrored to a stable internal location so `PPU_SDK_URL` stops being a rotating
  presigned link that has to be re-obtained from T-Head for every rebuild? That expiry, not secrecy, is the cost.
- Do graphs actually escape the PID loop on real workloads? Determines whether instantiate-time node accounting
  is ever needed.
- Is `HGGC_INJECTION64_PATH` a cleaner official route than the `dlsym` hook — what is its load ordering, can it
  rewrite return values, and does it affect `ppu-smi` (which does not load `libhggc.so`)?
- Given that no host library injection is required, does THead still need an entry in
  `_ManufacturerAcceleratableRuntimeNameMap`? Confirm on hardware before adding one.
- THead's MIG and vGPU surfaces are complete and already bound in `binding/hgml`. Should physical partitioning
  be prioritised over logical slicing?
