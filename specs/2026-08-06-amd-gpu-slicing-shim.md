# Spec: AMD GPU slicing shim — the library logical slicing needs, and the evidence it works

Status: Building
Type: Feature

> **This spec ships the library, not the capability.** `libvrocm.so`'s sources, the feasibility gates that
> prove the mechanism on real hardware, the compute-quota contract the operator side must satisfy, and the
> verification surface that keeps proving all of it. Turning it into something a user can request — the
> detector's `Status.LogicalSliced`, the allocator's `Sliced` server and its injection, the mask-derivation
> code in Go, and the `pack/` stage that builds and stages the library — is handed off, deliberately and for
> stated reasons; see Non-Goals. Until that lands, a slice happens only where somebody sets `LD_PRELOAD` and
> `HSA_CU_MASK` by hand, which is how every case here runs it.

## Summary

GPUStack Operator discovers and allocates AMD GPUs in exclusive and shared modes, but offers no **logical
(software) slicing** for them: `pkg/devicemanager/detector/amd/device.go` never fills `Status.LogicalSliced`,
`pkg/devicemanager/allocator/amd/deviceplugin.go` registers no `Sliced` server, and `pack/` contains no builder
stage for an AMD interception library. AMD is the last of the nine supported manufacturers without it.

This spec establishes that logical slicing for AMD is feasible on **both** consumer (RDNA) and datacenter
(CDNA) parts, fixes the mechanism it must use, and builds `libvrocm.so`: a per-card VRAM quota over a
cross-process ledger that doubles as the usage surface a metrics scraper will later read. The **compute**
dimension is not enforced by this library at all — ROCm enforces it in hardware through a CU mask — so what
this spec ships for compute is the **contract** that makes that mask correct, plus the self-check that catches
it when it is not.

Every claim below was measured on real hardware, on **two** machines that between them cover both AMD compute
architectures:

- **RDNA** — a 2× Radeon RX 7800 XT host (`gfx1101` / RDNA3 / Navi32, 60 CU · 30 WGP · 3 SE · 1 XCC · 16 GiB,
  ROCm 7.2).
- **CDNA** — a single-card Instinct MI300X host (`gfx942` / CDNA3, 304 CU · 32 SE · **8 XCC** · 192 GiB,
  ROCm 7.2.4, SPX compute partition / NPS1 memory partition, running under SR-IOV).

Four findings are load-bearing:

- **The compute mask fails open, silently, in four distinct ways — and which ones apply depends on the
  architecture.** A mask whose bits all fall outside the addressable width and a `GPU_list` written as a UUID
  are each discarded on both architectures; a CU set that splits a WGP pair is discarded on RDNA only; and on
  a multi-XCC part, a mask that does not place a bit in **every** XCC leaves the XCCs it missed running
  **unmasked**. The failure mode of a mis-computed quota is never an error — it is the silent loss of
  isolation, in whole or in part.
- **CU-mask semantics diverge between the two architectures, and the difference is structural, not a tuning
  constant.** On RDNA the allocation atom is a WGP (two CUs) and the mask must be aligned to the shader-engine
  count. On CDNA there is no WGP pairing at all — single-CU and odd masks are accepted — but the bits are
  **interleaved across XCCs**, which makes the atom `NUM_XCC` CUs and makes full XCC coverage mandatory.
  A derivation written for one architecture is not conservative on the other; it is wrong on it.
- **A `hipMalloc`-level ledger is not a memory quota.** Under a 2 GiB cap, `hipMalloc` correctly stopped at
  2.000 GiB and `hipMallocFromPoolAsync` then took a further 10 GiB on RDNA and 50 GiB on CDNA from the same
  card — in both cases the figure is where the test loop stopped, not where the card did. It is a first-class
  public API that PyTorch's mempool path uses.
- **A plain `LD_PRELOAD` is sufficient, and the library is ROCm-version-agnostic.** Both candidate mechanisms
  were built and measured against the same workload under the same quota. `LD_PRELOAD` enforces a 4 GiB cap
  end to end in PyTorch — `total_memory` and `mem_get_info` both report it and allocation stops at exactly
  4.000 GiB — with no recursion, no deadlock and no failed initialisation, **provided** it resolves the real
  entry points through `dlopen(RTLD_NOLOAD)` as well as `RTLD_NEXT`: a framework that `dlopen`s
  `libamdhip64`, as PyTorch does, puts them out of `RTLD_NEXT`'s reach. Symbol versioning is not an obstacle.
  And because the library links no ROCm object, one build serves every ROCm version — measured, a
  `7.2`-built object interposes cleanly inside a ROCm `6.4` container.

This spec is the tracked record of that evidence: every measurement it depends on is reproduced inline, in the
gate that produced it, rather than cited to a working note.

## Motivation

### Goals

- **Confirm the interception mechanism with evidence, not inference.** Prove on real hardware that a
  container-scoped interception library makes a workload see a VRAM quota instead of the physical card, and that
  it does so inside a real inference image rather than a synthetic one.
- **Fix the design decisions** so the implementation does not re-litigate them: interception layer, module
  split, the symbol surface each quota dimension needs, the ledger's layout, and where each artifact lives.
- **Close the memory surface, not just its front door.** The request API's `.sliced.memory-*` dimension is a
  cap on device memory, so a library that charges `hipMalloc` and ignores the stream-ordered allocator turns a
  2 GiB request into a whole card with nothing failing. Both the classic and the pool allocation families are
  charged against a **per-card** figure, because the allocator hands out one figure per card and a container
  may hold several.
- **Make the reported capacity self-consistent.** A workload that reads its budget from
  `hipDeviceProp_t.totalGlobalMem` must see the quota, not the card. Three exported entry points reach that
  number and only one of them is the obvious one.
- **Fix the compute-quota contract and make its violation detectable.** The mask derivation is specified here
  as a conformance table per architecture that the handed-off Go must satisfy, and `tools/` ships the probe
  that decides, on the node, whether a mask actually took effect — because the platform will not say so, and
  because on multi-XCC hardware a mask that leaks most of the card still measures as a working slice.
- **Cover both compute architectures, not the one that was easy to get.** RDNA and CDNA disagree on the
  allocation atom, on what `multiProcessorCount` means, and on which mask constructions fail open. Every
  compute claim in this spec is measured on one card of each, and the derivation is parameterised on the
  queried `NUM_XCC` rather than on an assumed SKU.
- **Make usage readable from outside the container.** The cross-process ledger is the only place a slice's
  memory usage exists in a form the operator can read: on consumer parts `amd-smi`'s per-process query reports
  VRAM but returns `CU_OCCUPANCY: N/A`, and no vendor tool carries the compute *limit* at all. Its layout is
  therefore a versioned contract rather than an implementation detail.
- Success is testable: (1) the library builds with `NEEDED` limited to `libc.so.6` and no `GLIBC_` requirement
  above the floor this spec fixes; (2) each PoC gate produces a recorded PASS/FAIL with captured output;
  (3) every interception symbol the module table names is re-established against a real `libamdhip64` and
  checked in as a regenerable manifest, not asserted as a count nobody can re-verify; (4) one container holding
  two cards with **different** VRAM quotas is held to each card's own figure; (5) a mask that violates the
  conformance table is caught by the self-check probe rather than by a user noticing their neighbour is slow.

### Non-Goals

- **The operator-side Go changes.** `detector/amd/device.go`'s `Status.LogicalSliced` and
  `allocator/amd/deviceplugin.go`'s `Sliced` server go with the `pack/` stage, not here: the Ascend precedent
  gates the advertised capability on the operator image having actually built the library
  (`detector/ascend/device.go:497-524`), so advertising it before the library exists would be a claim the node
  cannot honour. **This includes `cumask.go`** — the mask is consumed by the allocator, never by the library,
  so it belongs with the injection work. What this spec owes that work is the conformance table in F4, which
  it must satisfy, and the probe that checks it did.
  - **What this does NOT exclude: the tree compiling itself for verification.**
    `csrc/amd/rocm-slicing-shim/build.sh` holds the translation-unit lists and flags, and the skill's
    `scripts/build.sh xbuild-amd-rocm` calls it. All three shipped backends have that separation; the
    alternative — recipes living in the verification cases — is what drifted for THead.
- **A `pack/` builder stage and a shipping build for `libvrocm.so`.** Same handoff, same reason.
- **Compute rate limiting inside the library.** Deliberate, and the measurement is why: the platform's CU mask
  already delivers a hard per-tenant ceiling *and* oversubscription, because masks may overlap and tenants
  sharing an overlap divide it fairly (measured: three tenants on one half-card mask settle at 17.5 / 17.1 /
  17.1 % of the card, reproducible across three runs). A launch-intercepting token bucket or PID loop would
  add a hot-path cost, a tuning surface, and an escape hatch, to re-derive a property the hardware already
  provides at zero measured overhead (two disjoint tenants aggregate to 100.9 % of a solo card). The door stays
  open: a preloaded definition owns the name for the process's whole life, so adding launch interception later
  is another entry in the same table and needs no new mechanism.
- **Physical slicing / SR-IOV.** Consumer parts cannot do it at all — `SPX/DPX/CPX` is XCD-based and a
  single-XCC die can never satisfy the driver's partition-mode preconditions — and `.partitioned*` is a
  separate family from `.sliced*` and a separate effort.
- **Turning software slicing into a security boundary.** It is cooperative isolation, and this spec measures
  exactly how cooperative: removing the preload restores the whole card's memory, `env -u HSA_CU_MASK` restores
  the whole card's compute, `HSA_CU_MASK_SKIP_INIT=1` does the same without removing anything, a musl workload
  ignores the mechanism entirely and silently, and any allocation that reaches the HSA layer or the kernel
  driver directly never crosses our wrapper by construction. Same posture as the other backends.
- **Virtualising `rocm-smi` / `amd-smi`.** They read sysfs and the DRM nodes rather than HIP, so a symbol-level
  interposer cannot reach them — measured: inside a container restricted to one card with a 4 GiB cap, they
  still report two cards at 16 GiB each. `tools/`'s reader exists because of this, not in spite of it.
- Changing how the operator discovers or allocates AMD cards in exclusive/shared mode.

## Proposal

Logical slicing for AMD is delivered as an audit library injected only into sliced *workload* containers,
enforcing a per-card VRAM quota and making that quota the number every ROCm query reports, while the compute
quota is delivered by the platform's own CU mask under a derivation contract this spec fixes. The operator side
reuses the existing per-vendor plumbing — a builder stage whose product lands under `${GPUSTACK_LIB_DIR}/amd/`,
the device-manager init container that stages that tree onto the host, and an allocator branch that mounts the
library and injects its environment.

Six things are settled ahead of implementation because the research changed the obvious answer:

- **The activation mechanism is a container-scoped `/etc/ld.so.preload`**, the same as every shipped preload
  backend in this repo. `LD_AUDIT` was the other candidate and was rejected on measurement, not on taste; the
  full comparison is in Alternatives. The one thing a preload cannot do is tell the calling object apart, and
  the measured exposure to that is narrow: across a probe's 52 allocation-family calls and a PyTorch run,
  the only symbol `libamdhip64` was observed calling into through the PLT is `hipMemGetInfo`, which is in the
  substitute-unconditionally class where a self-call is harmless. **The risk is therefore managed rather than
  designed around** — see F1's caller-origin diagnostic and the escalation path in Risks.
- **Resolving the real entry point needs two lookups, not one.** `dlsym(RTLD_NEXT, …)` finds nothing when the
  framework `dlopen`s `libamdhip64` instead of linking it, which is what PyTorch does — measured, and it was
  the whole of an apparent "ROCm breaks under preload" result until it was traced. The fallback is
  `dlopen("libamdhip64.so", RTLD_NOLOAD | RTLD_LAZY)` followed by `dlsym` on that handle. A resolve that fails
  both ways must abort loudly and must never invent a return code: `1` is `hipErrorInvalidValue`, so a shim
  that returns it on a resolve miss is indistinguishable from a runtime that rejected the arguments.
- **The interception point is the HIP runtime layer** (`libamdhip64`), and the boundary that creates is
  acknowledged rather than papered over: `hsa_amd_memory_pool_allocate` and a direct `AMDKFD_IOC_*` `ioctl`
  are defined by other objects and never reach our wrappers.
- **One build serves every ROCm version.** The library links no ROCm object — measured, `DT_NEEDED` is
  `libc.so.6` alone and its undefined-symbol set contains zero `hip*`/`hsa*` names — because every real entry
  point is reached through `dlsym` at run time rather than through a link-time dependency. A `7.2`-built
  object was measured interposing correctly inside a ROCm `6.4`
  container. So `pack/` needs **one** builder stage, not the per-runtime-major fan-out NVIDIA
  (`cuda-12`/`cuda-13`) and Ascend (`cann-<major>-<family>`) need.
- **The glibc floor is a design choice, and this spec fixes it at `GLIBC_2.4`.** Two things would raise it to
  `2.34` and lock out Ubuntu 20.04 and RHEL 8 workload images: reaching for a process-shared `sem_t` or
  `pthread_once`, and — less obviously — the resolver's own `dlopen`/`dlsym`, which glibc moved into `libc` at
  that version and which were measured to be the only two symbols over the line. So the ledger's
  synchronisation is `fcntl` record locking plus compiler atomics, and the two loader calls are version-pinned
  with `.symver`. Both are constraints on the implementation, not preferences.
- **Compute is oversubscribable, so `CoresPercentageOvercommit` is `true`.** Masks may overlap; a tenant's own
  mask is a hard ceiling even when the card is otherwise idle (a half-card mask alone still measures 50.5 %),
  and tenants sharing an overlap divide it fairly — measured on both architectures. The allocation policy that
  follows is *pack disjointly while capacity allows, overlap only when oversubscribed* — hard isolation up to
  100 %, graceful degradation beyond.
- **The mask derivation branches on `NUM_XCC`, and neither branch degrades safely into the other.** RDNA
  allocates in WGP pairs aligned to the shader-engine count; CDNA has no pairing but interleaves mask bits
  across XCCs, which makes the atom `NUM_XCC` CUs and makes covering every XCC mandatory — a mask that misses
  one leaves that XCC unmasked and was measured handing a container 267 of 304 CUs while reporting a
  believable 3.7 % of throughput. Because of that last part, **the self-check must judge by occupancy, not by
  throughput**; see F4.

### User Stories

#### Story 1

As a GPUStack Operator maintainer, I want the interception mechanism, the symbol surface and the compute-mask
contract confirmed against real hardware before the injection work starts, so that the operator side is built
against a verified mechanism instead of an inference.

#### Story 2

As a platform user running a workload on a sliced AMD card, I want every capacity number my framework reads —
`hipMemGetInfo`, `hipDeviceProp_t.totalGlobalMem`, `hipDeviceTotalMem` — to report my quota rather than the
physical card, so that the framework sizes itself to what it may actually use instead of OOM-ing at 25 % of
what it was told it had.

#### Story 3

As a platform user sharing one AMD card with other workloads, I want an allocation past my quota to be refused
no matter which allocator my framework reaches for, so that a neighbour using the stream-ordered allocator
cannot take memory my slice was promised.

#### Story 4

As an operator developer bringing up a new AMD node, I want a probe that tells me whether the compute mask this
node actually honours matches what the scheduler assumed, so that a card whose mask silently does nothing is
reported at detect time rather than discovered as a noisy-neighbour incident.

#### Story 5

As an operator developer verifying a build change, I want `gpustack-operator-xbuild-and-verify` to carry an AMD
backend with numbered cases, so that the mechanism keeps being re-proved after every change rather than once.

### Core Features & Acceptance Criteria

#### F1 — Interception mechanism and its measured constraints

- Activation is a container-scoped `/etc/ld.so.preload` naming `libvrocm.so`, bind-mounted read-only by the
  allocator — the same shape the four existing preload backends use, and the same
  `rootfs/etc/gpustack/lib/<vendor>/ld.so.preload` asset pattern. The file's single line and the allocator's
  in-container mount path are one contract; they must not drift.
- The library defines each intercepted HIP entry point under its **plain, unversioned** name. Symbol
  versioning is not an obstacle — measured, a plain definition interposes `libamdhip64`'s
  `@@hip_x.y`-tagged exports without a version script.
- **Resolution is two-step and fail-loud.** `dlsym(RTLD_NEXT, name)` first; on a miss,
  `dlopen("libamdhip64.so", RTLD_NOLOAD | RTLD_LAZY)` and `dlsym` that handle. A `dlopen`ing framework — which
  PyTorch is — leaves the real symbol outside `RTLD_NEXT`'s scope, and a shim that silently substitutes an
  error code there is worse than one that stops: the natural placeholder `1` **is** `hipErrorInvalidValue`, so
  the framework reports a plausible argument error and the real cause never surfaces. A resolve that fails both
  ways aborts with the symbol name.
- **A caller-origin diagnostic ships with the product**, and it is not decoration. A preload cannot refuse to
  fire on a call the HIP runtime makes into its own exported symbol, so the mitigation is visibility: at log
  level 2 each wrapper reports the calling object, resolved with `dladdr` on `__builtin_return_address(0)`,
  the first few times it fires. If a future ROCm release starts self-calling an allocation entry point, it
  appears as a diagnostic line naming `libamdhip64` rather than as a double charge nobody can explain. The
  measured baseline this guards is recorded in F2 Gate 7.
- **The allocation family must be re-entrancy-safe regardless.** Whether a re-entrant call comes from the
  runtime or from a workload's own nested use, charging it twice is a bug; the in-process lock is
  re-entrancy-counted rather than deadlocking, and a charge is recorded exactly once per returned pointer.
- **No `pthread_mutex`, no `pthread_once`, no `sem_t`** — and the reason is now purely the glibc floor, not
  loader re-entrancy: those symbols carry `GLIBC_2.34`, which would lock the product out of Ubuntu 20.04 and
  RHEL 8 workload images. In-process mutual exclusion is a GCC atomic spinlock; cross-process is `fcntl`
  record locking. Both are available at `GLIBC_2.4`.
- **Every loader call the library makes must be version-pinned, or the glibc floor is lost to the mechanism
  itself.** glibc moved `libdl` into `libc` at `GLIBC_2.34`, so on any build host newer than that these
  symbols bind at 2.34 and become the *only* ones above the floor — measured, and they are the whole gap.
  Three `.symver` directives close it, and the third is easy to miss because it belongs to the diagnostic
  rather than to the resolver:

  ```c
  __asm__(".symver dlopen,dlopen@GLIBC_2.2.5");
  __asm__(".symver dlsym,dlsym@GLIBC_2.2.5");
  __asm__(".symver dladdr,dladdr@GLIBC_2.2.5");   /* the caller-origin diagnostic */
  ```

  Measured on a glibc 2.35 host: with only the first two the ceiling drops to `GLIBC_2.4` until the
  caller-origin diagnostic is added, at which point `dladdr` pushes it straight back to `GLIBC_2.34`. With all
  three the ceiling is `GLIBC_2.4`, `NEEDED` stays `libc.so.6` alone, and the shim still resolves and enforces
  at run time. Verified running, not merely linking — a pin that links but breaks resolution would be the
  worst of both. The rule to carry forward is the general one: **any new `libdl` call needs its own pin**, and
  the build-time assertion is what catches the one that does not get it.
- The constructor must not touch the ledger. The region is mapped lazily on first memory operation, so a
  container that never allocates never creates one.
- Acceptance: `nm -D` on the product exports only the intercepted HIP names and no `la_*` symbol;
  `objdump -p` shows `NEEDED libc.so.6` and nothing else; `objdump -T` shows no `GLIBC_` above `GLIBC_2.4`;
  the undefined-symbol set contains no `hip*` or `hsa*` name. All four are asserted at build time and
  re-checked by case 1, which needs no GPU.

#### F2 — PoC gates (all met; recorded output below)

Gates 1-7 ran container-scoped on a 2× `gfx1101` host, ROCm 7.2, inside `gpustack/runner:rocm7.2-vllm0.25.1`
(PyTorch 2.11.0, glibc 2.35) unless stated. Gate 8 ran on the `gfx942` host, ROCm 7.2.4, glibc 2.35. All are
read-only with respect to their host.

- **Gate 1 — the mechanism works on consumer silicon.** A container-scoped interception library reports a
  configured quota through `hipMemGetInfo` and refuses allocation past it. **PASS** — under a 4 GiB cap,
  `hipMemGetInfo` reported `free=4.000 total=4.000 GiB` while a second, uncapped card in the same container
  reported the true `15.805 / 15.984 GiB`; 256 MiB chunked allocation returned `hipErrorOutOfMemory` at exactly
  the 16th chunk. End to end in PyTorch, the framework's own error names the virtual capacity:
  `CUDA out of memory … GPU 0 has a total capacity of 4.00 GiB`.
- **Gate 2 — memory-path completeness. PASS, and it found the hole this library exists to close.** Under a
  2 GiB cap with only the classic allocator charged:

  ```
  [1] hipMalloc               -> 2.000 GiB   (ledger stopped here, correctly)
  [2] hipMallocFromPoolAsync  -> 10.000 GiB  EXTRA — loop cap, not a limit
  [3] hipMallocAsync          -> 0.000 GiB   (charged, correctly refused)
  ==> total device memory held = 12.000 GiB against a 2 GiB quota
  ```

  `hipDeviceGetDefaultMemPool` returns a usable pool with no special privilege. The 10 GiB is the test's own
  40-chunk ceiling; the real bound is physical VRAM.
- **Gate 3 — the reported-capacity surface. PASS, and the obvious symbol is the wrong one.** `libamdhip64`
  exports three property entry points — `hipGetDeviceProperties@@hip_4.2`,
  `hipGetDevicePropertiesR0600@@hip_6.0`, `hipGetDevicePropertiesR0000@@hip_4.2` — plus
  `hipDeviceTotalMem@@hip_4.2`. ROCm 6+ headers macro-map the plain name onto `R0600`, and a tracer bullet
  registering **both** logged only `R0600` ever binding. With `hipMemGetInfo` alone virtualised,
  `torch.cuda.get_device_properties(0).total_memory` read `15.984 GiB` against a 4 GiB cap; with `R0600` also
  interposed it read `4.000 GiB`, agreeing with `mem_get_info()`. `hipDeviceProp_t` is 1472 bytes with
  `totalGlobalMem` at offset 288 and `multiProcessorCount` at 388.
- **Gate 4 — the compute mask's three RDNA fail-open modes. PASS (all three reproduce).** On a 60 CU / 30 WGP
  single-XCC card:

  | `HSA_CU_MASK` | composition | measured | verdict |
  | --- | --- | --- | --- |
  | `0:0-13` | 7 whole WGP pairs | 21 % | throttled |
  | `0:2-15` | 7 whole pairs, offset start | 21 % | throttled |
  | `0:16-29` | 7 whole pairs | 21 % | throttled |
  | `0:0-14` | 7 pairs **+ orphan CU 14** | **100 %** | **fail-open** |
  | `0:1-14` | orphan at both ends | **100 %** | **fail-open** |
  | `0:15-29` | orphan CU 15 + 7 pairs | **100 %** | **fail-open** |

  Pair *alignment* decides it, not the element count's parity — `0:2-15` is valid and `0:1-14` is not.
  Reproduced on a real PyTorch fp16 GEMM, where `0:0-29` measured 36.2 TFLOP/s and `0:0-30` measured
  55.8 against an unmasked 55.5. The other two modes: a `ROC_GLOBAL_CU_MASK` whose set bits all fall at or
  above the WGP count yields the whole card (`0xFFFFFFFC0000000` → 100 %), and an `HSA_CU_MASK` whose
  `GPU_list` is written as a `GPU-<hex>` UUID is discarded outright (99 %).
- **Gate 5 — compute quota semantics. PASS.** Barrier-synchronised 8 s soak, ILP-saturating kernel, full card
  = 20149 GFLOP/s:

  | scenario | per tenant | total |
  | --- | --- | --- |
  | one tenant, 50 % mask, card otherwise idle | 50.5 % | — |
  | disjoint 50 % + 50 % | 50.4 / 50.4 | 100.9 % |
  | identical 50 % masks (both on the same half) | 25.8 / 25.8 | 51.5 % |
  | four tenants, unmasked (400 % oversubscribed) | 25.4 each | 101.8 % |
  | three tenants, identical 50 % masks | 17.5 / 17.1 / 17.1 | 51.7 % |
  | 20 % capped + uncapped | 18.9 / 80.8 | 99.7 % |

  The quota is a ceiling that holds against an idle card; overlap is permitted and shares fairly; disjoint
  partitioning costs nothing. Fairness is reproducible — the three-tenant row is identical across three runs.
  **A methodology note that is part of the result:** without a start barrier the same matrix reports two
  100 %-masked tenants aggregating to 200 % of the card's peak, which is impossible; and a latency-bound kernel
  under-fills a small partition and inflates the overlap rows. Both mistakes were made and corrected. Any case
  measuring this must synchronise its start and saturate.
- **Gate 6 — lifecycle and version reach. PASS.** A process killed with `SIGKILL` while holding 4 GiB of a
  6 GiB quota leaves nothing behind: the next process is granted the full 6 GiB, twice in a row. And a
  `7.2`-built library interposed correctly inside a ROCm `6.4` container (3 GiB cap honoured), while the mask
  semantics — including all of Gate 4's fail-open behaviour — are byte-for-byte the same under ROCm 6.4 as
  under 7.2.
- **Gate 7 — mechanism selection. PASS, and it overturned the design's first answer.** Both mechanisms were
  implemented and run against the same PyTorch workload under the same 4 GiB cap.

  | | measured |
  | --- | --- |
  | plain preload interposes a `@@hip_x.y`-versioned export | **yes** — no version script needed |
  | `libamdhip64` self-calls an exported symbol through the PLT | **yes, exactly one**: `hipMemGetInfo`. Zero across 52 allocation-family calls |
  | preload enforces the cap end to end in PyTorch | **yes** — `total_memory` 4.000, `mem_get_info` 4.000, OOM at 4.000 GiB |
  | preload survives HIP initialisation | **yes** — no recursion, no deadlock, no init failure |

  The first attempt appeared to prove the opposite — PyTorch died with `hipErrorInvalidValue` — and that
  result was **wrong for a reason worth recording**: `dlsym(RTLD_NEXT, "hipGetDevicePropertiesR0600")` misses
  under a `dlopen`ing framework, and the shim's own placeholder return of `1` on that miss *is*
  `hipErrorInvalidValue`. Two defects stacked into a convincing false negative. With the `RTLD_NOLOAD`
  fallback the same shim enforces the cap cleanly. **The single self-calling symbol is in the
  substitute-unconditionally class**, so the one capability `LD_AUDIT` uniquely offers — refusing to fire on a
  runtime-internal call — currently has nothing to do.
- **Gate 8 — datacenter silicon. PASS on the memory dimension, PASS-with-a-new-defect on the compute one.**
  A `gfx942` card, 304 CU · 32 SE · 8 XCC · 192 GiB, SPX/NPS1, under SR-IOV.

  *Memory — everything transfers unchanged.* `hipDeviceProp_t` is byte-identical to the RDNA host (1472 bytes,
  `totalGlobalMem` at 288, `multiProcessorCount` at 388), so the regression fixture is one fixture, not two.
  All three reported-capacity entry points virtualise together: under a 4 GiB cap,
  `prop.totalGlobalMem = memGetInfo.total = hipDeviceTotalMem = 4.000 GiB`, and chunked `hipMalloc` stopped at
  exactly 4.000 GiB (2.000 under a 2 GiB cap). The pool hole reproduces and is worse in absolute terms: with
  the pool family deliberately un-wrapped, a 2 GiB cap held **52.000 GiB** — 2.000 through `hipMalloc` and
  50.000 more through `hipMallocFromPoolAsync`, again the test loop's ceiling rather than the card's. With the
  family wrapped, the same run adds `+0.000 GiB`. The cross-process ledger behaves: against one 4 GiB card
  quota, a process holding 3 GiB left the next process exactly 1 GiB (`total=4.000 free=0.000`), and once the
  first exited the next was granted the full 4 GiB.

  *Memory — and three more open doors than the RDNA gates found.* Gate 2 established the pool family as a
  bypass; a discriminating re-run here — a **256 MiB** quota against **512 MiB** requests, so a charged entry
  must fail and an uncharged one cannot — shows the classic family is not one door either:

  | entry point | 256 MiB quota, 512 MiB request | reading |
  | --- | --- | --- |
  | `hipMalloc` | `out of memory` | charged |
  | `hipMallocAsync` | `out of memory` | charged |
  | `hipMallocFromPoolAsync` | `out of memory` | charged |
  | `hipMallocManaged` | **OK** | **uncharged — bypass** |
  | `hipExtMallocWithFlags` | **OK** | **uncharged — bypass** |
  | `hipMallocPitch` | **OK** | **uncharged — bypass** |
  | `hipHostMalloc` | OK | host memory, correctly not charged |
  | `hipMallocArray` | `operation not supported` | unavailable on this part at all |

  The shim under test wrapped only `hipMalloc` and the pool family, so this is what T2's symbol list is
  *for* — measured rather than assumed. The control that makes the table trustworthy is the same run with the
  pool wrappers removed, where `hipMallocAsync` and `hipMallocFromPoolAsync` flip from `out of memory` to
  `OK` and nothing else moves. `hipMallocArray` being unsupported on `gfx942` means the array entries cannot
  be exercised on CDNA and their coverage rests on the RDNA host.

  *Build constraints — one new requirement, found here.* On a glibc-2.35 build host the library's own loader
  calls raise the floor: `dlopen`, `dlsym` and `dladdr` moved into `libc` at `GLIBC_2.34`, and they were the
  **only** symbols above `GLIBC_2.4` in the product. The `.symver` pins in F1 restore a `GLIBC_2.4` ceiling
  with `NEEDED libc.so.6` alone, and the shim still resolves and enforces correctly at run time — verified,
  not merely linked. `dladdr` was found the second way round: the floor was clean until the caller-origin
  diagnostic was added, and adding it silently put the ceiling back to `2.34`.

  *A real framework, and a ROCm major it was not built against.* PyTorch **2.9.1+rocm6.4** was installed on
  this ROCm **7.2.4** host, so its bundled `torch/lib/libamdhip64.so` is a 6.4 runtime. A shim compiled
  against 7.2.4 headers interposed it and enforced a 4 GiB cap end to end:

  ```
  get_device_properties.total_memory = 4.000 GiB
  mem_get_info                       = free 4.000 / total 4.000 GiB
  allocation stopped at                4.000 GiB
  framework error: HIP out of memory. Tried to allocate 256.00 MiB.
                   GPU 0 has a total capacity of 4.00 GiB of which 0 bytes is free.
  ```

  That single run is Gate 1 and Gate 6's cross-version claim at once, on the other architecture and against a
  **framework-bundled** runtime rather than the system one. The Gate 3 control arm reproduces here too: with
  only `hipMemGetInfo` virtualised, `get_device_properties.total_memory` reported the physical
  **191.984 GiB** while `mem_get_info` reported 4.000 — the framework contradicting itself, exactly as on
  RDNA, and the reason `hipGetDevicePropertiesR0600` and `hipDeviceTotalMem` are not optional.

  *Caller origin — the preload's one weakness has even less to do here.* Traced across a full PyTorch
  allocate-to-OOM run, every call into the wrappers came from `libc10_hip.so` (18 × `hipMalloc`),
  `libtorch_python.so`, `libtorch_hip.so` and `libmagma.so`. **Zero originated in `libamdhip64` itself** — not
  even `hipMemGetInfo`, which is the one self-call the RDNA gate found. Activation cost is `+4 ms` on process
  start (`+2.2 %`).

  *SIGKILL, as a negative control.* The shim under test deliberately shipped **no** liveness sweep, and the
  charge leaked exactly as predicted: a process killed while holding 4 GiB of a 6 GiB quota left the next
  process able to claim only 2 GiB. That is the behaviour T1's sweep exists to prevent, measured as a failure
  rather than argued for. `rocm-smi` remains outside the quota's reach on this architecture too, reporting the
  full 206141652992 bytes under a 4 GiB cap, and `env -i` still restores the physical figure.

  *Compute — the derivation does not transfer, and the reason is a fail-open mode that does not exist on RDNA.*
  The bit→hardware mapping was read from inside the running kernel via `HW_ID`/`XCC_ID` rather than inferred
  from throughput, which is what made the defect visible: throughput alone says the mask works.

  | `HSA_CU_MASK` | CUs occupied | per-XCC | throughput | verdict |
  | --- | --- | --- | --- | --- |
  | *(none)* | 304 | 38 × 8 | 100 % | reference |
  | `0:0-7` | 8 | 1 each | 3.7 % | correct |
  | `0:0-15` | 16 | 2 each | 7.4 % | correct |
  | `0:0-31` | 32 | 4 each | 13.9 % | correct |
  | `0:0-151` | 152 | 19 each | 56.9 % | correct |
  | `0:0-37` | 38 | 5,5,5,5,5,5,4,4 | 13.9 % | **6 CUs held, 0 delivered** |
  | `0:0` | **267** | 1,38,38,38,38,38,38,38 | 3.7 % | **fail-open on 7 XCCs** |
  | `0:0-3` | **156** | 1,1,1,1,38,38,38,38 | 3.7 % | **fail-open on 4 XCCs** |
  | `0:0,8,16,24` | **270** | 4,38,38,38,38,38,38,38 | 13.7 % | **fail-open on 7 XCCs** |
  | `0:304-400` | 304 | 38 × 8 | 100 % | fail-open (as on RDNA) |
  | `GPU-<hex>:0-37` | 304 | 38 × 8 | 100 % | fail-open (as on RDNA) |

  The three highlighted rows are the finding: **a throughput measurement cannot detect them.** `0:0` reads as
  a working 3.7 % slice because the makespan is set by the most-constrained XCC, while 267 of the card's 304
  CUs are in fact reachable by that container. WGP pairing, by contrast, simply does not exist here —
  `0:1`, `0:1-2`, `0:0,2,4,6` and `0:1,3,5,7` were all accepted and honoured exactly.
  `HSA_CU_MASK_SKIP_INIT=1` removes the mask on this architecture too.

  *Multi-tenant.* Barrier-synchronised 8 s soak, solo baseline for an 8-CU slice = 4208 GFLOP/s:

  | scenario | per tenant | CUs occupied each |
  | --- | --- | --- |
  | XCC-covering disjoint — `0:0-7` + `0:8-15` | 4250 / 4223 | 8 / 8 — no overlap |
  | naive bit-split — `0:0-3` + `0:4-7` | 4233 / 4236 | **156 / 156 — 152 shared** |
  | identical masks — `0:0-7` twice | 2138 / 2140 | 8 / 8 |

  The middle row is the one to read twice: the throughput column looks healthy, and the isolation is gone.
  The last row confirms that overlap shares evenly on CDNA as it does on RDNA, so the
  `CoresPercentageOvercommit = true` policy holds on both.

  *The same thing on a real workload, where it looks even more convincing.* A PyTorch fp16 4096² GEMM, with
  autotuning warm-up kept outside the timed window:

  | mask | CUs asked for | measured | share |
  | --- | --- | --- | --- |
  | *(none)* | 304 | 573.3 TFLOP/s | 100 % |
  | `0:0-303` | 304 | 566.8 | 98.9 % |
  | `0:0-151` | 152 | 319.4 | 55.7 % |
  | `0:0-31` | 32 | 109.6 | 19.1 % |
  | `0:0-3` | *(fail-open, 156 held)* | 29.2 | 5.1 % |
  | `0:0` | *(fail-open, 267 held)* | 28.1 | 4.9 % |

  Two things to take from it. Real tensor work is **sublinear in CU count in the tenant's favour** — 10.5 % of
  the CUs delivers 19.1 % of the throughput, and half the CUs deliver 55.7 % — so converting a percentage
  quota to CUs linearly under-states what the tenant will actually get, the same way it does on RDNA.
  And the two fail-open masks land at the **bottom** of the table: to anyone reading throughput, `0:0` looks
  like the tightest slice on offer while its container can reach 267 of the card's 304 CUs. `multiProcessorCount`
  stays 304 under every `HSA_CU_MASK` row, so it is no help either.

  *Compute masks do not partition memory bandwidth.* Two tenants on XCC-covering disjoint halves
  (`0:0-151` and `0:152-303`), 8 s barrier-synchronised soak:

  | scenario | compute tenant | bandwidth tenant |
  | --- | --- | --- |
  | each alone on its half | 64469 GFLOP/s | 3522 GB/s |
  | both halves running compute | 55273 | — |
  | compute half **+ bandwidth-saturating half** | **41453** | 3430 GB/s |

  Against a compute neighbour the tenant keeps 85.7 % of its solo figure — that drop is the card's clocks
  being shared, not interference, since the pair aggregates to 101.4 % of the full card's solo throughput.
  Against a **bandwidth-saturating** neighbour it keeps only 75 % of *that*, a further 25 % gone, while the
  bandwidth tenant loses almost nothing (97.4 %). And bandwidth barely scales with the mask at all: 152 CUs
  reach 3522 GB/s against 3605 GB/s for the whole card — **97.7 % of the card's bandwidth from half its
  compute quota**. A CU mask is a compute ceiling and nothing else.
- Acceptance (**met — all eight gates PASS**): each gate produces a PASS/FAIL row with captured output; a FAIL
  is a recorded finding, not a silent retry.

#### F3 — Module design

| Module | Responsibility | Fixed constraints |
| --- | --- | --- |
| `common/` | Quota parsing, cross-process ledger, locking, logging, usage region | No `hip*`/`hsa*` type may appear, which is what makes it testable with no ROCm and no device. Synchronisation is `fcntl` record locks plus GCC atomics — never `pthread_*` or `sem_t`, because they carry `GLIBC_2.34` and would raise the floor (F1). The lock is held until the real allocation returns, and is re-entrancy-counted. The quota is re-read on attach rather than frozen by whichever process created the region. |
| `hip/` | The interposed entry points and the resolver | Covers the allocating family **including the stream-ordered/pool entries** (Gate 2), the freeing family, and the reported-capacity family **including `hipGetDevicePropertiesR0600`** (Gate 3). One resolver serves every wrapper: `RTLD_NEXT`, then `dlopen(RTLD_NOLOAD)`, then abort — never a fabricated return code. Every entry carries the caller-origin diagnostic. |
| `tools/` | Readers, preloaded into nothing | `rocm-monitor` reads the ledger's region for the operator, because no vendor tool carries the compute limit and `amd-smi`'s per-process query returns `CU_OCCUPANCY: N/A` on consumer parts. `rocm-cumask-check` is the F4 self-check. Neither may link the ledger's mapping code — that code creates a region and takes the card's lock, and a reader must do neither. |
| `testing/` | Gate-only artifacts | Never shipped in the library; the boundary between product and scaffolding is a directory rather than a comment. |
| *(handed off)* detector | Advertise `Status.LogicalSliced` | Gate on facts the device-manager can see, and on F4's self-check having passed for the card. |
| *(handed off)* allocator | Inject into sliced containers | F5's environment, F4's mask, and the three-variable tuple rule. |

- **The allocation tracker's failure mode is fail-closed.** A fixed-capacity pointer→(card, bytes) table that
  silently drops an insert leaks the charge forever: the matching free finds no entry, refunds nothing, and the
  quota shrinks monotonically for the life of the container. Insert failure is an error, not a log line.
- **The key→(card, bytes) map stays process-local and must.** A device pointer is a value in one process's
  address space, so two processes may legitimately hold the same one on different cards; a shared table keyed
  on it would let one process's free refund another's allocation. What is shared is the per-card total.
- **Admission and charge happen under one lock acquisition.** Checking the quota, calling the real allocator
  and recording the charge must not be three separately-locked steps: two processes that both pass the check
  before either charges will both allocate, and the card is over quota until one frees.
- **A process that dies holding a charge does not shrink the quota forever.** Its slot is swept by liveness on
  the path that would otherwise refuse, so the sweep costs nothing on the hot path. Gate 6 measures this.
- **The pool family is charged, not merely counted.** Gate 2 is the reason: it is the one family whose omission
  turns a quota into a suggestion. The mirror rule also applies — an *imported* pool pointer maps an allocation
  another process already paid for and must not be recorded, or this container's free would credit it for
  memory it never took.
- Acceptance: every symbol the module table names is re-established against a real `libamdhip64` — the one
  inside the build image, not a host copy — and checked in as
  `references/amd-hip-symbol-manifest.md` carrying the names, their version tags, the per-symbol substitution
  policy, the image digest and the command that regenerates it. A count that cannot be reproduced is not
  evidence.

#### F4 — The compute-quota contract

The library does not enforce compute. This feature is the contract that makes the platform's enforcement
correct, plus the artifact that detects when it is not.

**Three separate things are easy to conflate here, so they are named before anything else. Computing a mask,
injecting it, and checking it took effect are three different jobs, done by three different pieces, and only
the third one ships in this spec.**

| | job | who does it | when |
| --- | --- | --- | --- |
| **Compute** | topology and a requested percentage → a mask string, by the arithmetic below | the Go allocator, `allocator/amd/cumask.go` | per allocation |
| **Inject** | emit `HSA_CU_MASK` into the container, alongside `ROCR_VISIBLE_DEVICES` and the memory-limit variables | the Go allocator's device-plugin `Allocate` | per allocation |
| **Enforce** | apply the mask to the workload's queues | **ROCr**, at its own init — not this library, which never touches compute | per process |
| **Check** | run a kernel, read the hardware back, decide whether the mask took effect | `tools/rocm-cumask-check` | once, at detection |

**The mask is derived, never discovered.** The arithmetic below is closed-form integer work over figures read
once from the HSA agent-info API: there is no probing, no trial launch, no measurement in the loop and no
fallback. Given 60 CU / 3 SE / 1 XCC and 50 %, the answer is `0:0-29` and nothing else; given 304 CU / 8 XCC
and 50 %, it is `0:0-151`. That is what makes the allocator's job testable against a table rather than against
a card.

**The check exists because deriving correctly and being obeyed are different questions, and the platform
answers neither.** A rejected mask produces no error, no log line and no changed return code — the container
simply gets the whole card. `0:0-14` looks like fifteen CUs and delivers sixty; `0:0` on `gfx942` reads as a
plausible 3.7 % of throughput while the container reaches 267 of 304 CUs. Nothing short of running a kernel
and reading the hardware separates *honoured*, *discarded* and *honoured on some XCCs only*.

**What this spec ships is the last row, and none of the first three.** `cumask.go` and the injection are
Non-Goals — the mask is consumed by the allocator and never by the library, so they land with that work, and
enforcement was never ours to write. What this spec owes that work is two things: the conformance tables
below, which its arithmetic must reproduce, and the probe that proves a card honours what they say.

**The derivation branches on `NUM_XCC`, and the two branches share no arithmetic.** This is the single most
consequential thing in this spec, because both branches are correct-looking and each is silently wrong on the
other architecture.

```
# --- RDNA branch (NUM_XCC == 1) -------------------------------------------
W = CU / 2                          # WGP count; on gfx1101, 60 CU -> 30 WGP
n = round(W * pct / 100)            # requested WGPs
n = max(S, floor(n / S) * S)        # align DOWN to a multiple of the shader-engine count, floor at one round
select n free WGPs (disjoint while capacity allows; overlap only when oversubscribed)
emit HSA_CU_MASK = "<idx>:" + ranges(expand each WGP w -> CUs {2w, 2w+1})

# --- CDNA branch (NUM_XCC > 1) --------------------------------------------
X = NUM_XCC                         # 8 on MI300X
K = CU / X                          # usable CUs per XCC; 304 / 8 = 38
n = round(CU * pct / 100)           # requested CUs
n = floor(n / X) * X                # align DOWN to a whole number of "one CU in every XCC" atoms
reject the request when n < X       # a sub-atom mask does NOT clamp -- it fails open
select n free mask bits, in whole atoms {b, b+X, ..} aligned to a multiple of X
emit HSA_CU_MASK = "<idx>:" + ranges(those bit indices)
```

Six rules, each with a measured failure behind it:

1. **Count in WGPs on RDNA, in CUs on CDNA.** `hipDeviceProp_t.multiProcessorCount` reports **30** on a 60-CU
   `gfx1101` — half the topology figure — but **304** on a 304-CU `gfx942`. The same field means different
   things on the two architectures, and picking the wrong one silently halves or doubles every quota. The same
   split shows in `ROC_GLOBAL_CU_MASK`, which rewrites `multiProcessorCount` to `popcount/2` on RDNA and to
   plain `popcount` on CDNA (measured: `0xFF` → 8, `0xFFFFFFFF` → 32).
2. **Emit whole WGP pairs — on RDNA only.** Gate 4: an orphaned CU discards the entire mask. On CDNA there is
   no pairing: `0:1`, `0:1-2`, `0:0,2,4,6` and `0:1,3,5,7` are all accepted and each honoured to the exact CU.
   Carrying the RDNA pairing rule onto CDNA does not fail — it just doubles every slice.
3. **Align to the shader-engine count — on RDNA.** Effective compute is `S × floor(n / S)` WGPs; the kernel
   distributes mask bits round-robin across shader engines, so a remainder produces no throughput at all.
   Measured across 18 sample points: 3 and 4 WGPs deliver the same, 6 and 8 deliver the same, 28 and 29
   deliver the same.
4. **Align to `NUM_XCC` and cover every XCC — on CDNA.** The mask bits are interleaved across XCCs before
   anything else advances. Read out of the hardware `HW_ID`/`XCC_ID` registers from inside the running kernel,
   the mapping on `gfx942` is exactly:

   ```
   bit i  ->  XCC = i mod X ,  SE = (i div X) mod (SE / X) ,  CU = i div (X * (SE / X))
   ```

   so eight consecutive bits are eight *different* XCCs, one CU each — never eight CUs of one XCC. Two
   consequences follow, and both were measured. First, a bit count that is not a multiple of `X` distributes
   unevenly and the remainder yields **nothing**: `0:0-37` (38 bits) gives 5 CUs to six XCCs and 4 to the other
   two, and delivers exactly the throughput of `0:0-31` (32 bits) — the extra six CUs are occupied, unusable,
   and unavailable to anyone else. Second, and far worse, **a mask that misses an XCC leaves that XCC entirely
   unmasked**: `0:0` — one bit, nominally 1 CU of 304 — was measured occupying **267 CUs**, because XCCs 1-7
   received no mask at all. `0:0-3` occupied 156. This mode has no RDNA analogue and no single-XCC test
   machine can surface it.
5. **Reject an under-sized compute request; never clamp it.** Because a sub-`NUM_XCC` mask fails open rather
   than rounding down, a request that lands below one atom cannot be honoured at all. On MI300X the smallest
   valid slice is 8 CUs of 304 — **2.63 %** — and the granularity above it is the same 2.63 %.
6. **Read topology from an API, not from KFD sysfs — for contract stability, not because sysfs lies.** Both
   sources agree, on both architectures: `array_count / simd_arrays_per_engine` equals the HSA-reported
   `NUM_SHADER_ENGINES` (RDNA: 6/2 = 3; CDNA: 32/1 = 32). What matters is that **both report device-wide
   counts that already include the XCC multiplier** — `NUM_SHADER_ENGINES` on MI300X is 32, meaning 4 SE per
   XCC × 8 XCCs, *not* 4 — so every per-XCC quantity must be obtained by dividing by `NUM_XCC` rather than
   read directly. `HSA_AMD_AGENT_INFO_NUM_SHADER_ENGINES` (0xA00C), `NUM_SHADER_ARRAYS_PER_SE` (0xA00D),
   `COMPUTE_UNIT_COUNT` (0xA002) and `NUM_XCC` (0xA111) are already in `binding/hsa/const.go`;
   `amdgpu_gpu_info.num_shader_engines` is already in `binding/amdgpu`. `amd-smi` exposes no shader-engine
   field. Prefer the HSA route: on the MI300X host, which runs under SR-IOV, `rocm-smi` could not complete a
   libdrm query at all (`get_name, Error when calling libdrm`) while the HSA agent-info path returned every
   field. Enumeration must also filter render nodes by driver; a host with an integrated GPU has DRM render
   nodes that are not `amdgpu`.

**Conformance table A** — RDNA, measured on a 60 CU / 30 WGP / 3 SE / 1 XCC card:

| requested | WGPs before align | after align | mask | measured share |
| --- | --- | --- | --- | --- |
| 10 % | 3 | 3 | `0:0-5` | ~11 % |
| 20 % | 6 | 6 | `0:0-11` | ~22 % |
| 25 % | 8 (7.5 → 8) | 6 | `0:0-11` | ~22 % |
| 50 % | 15 | 15 | `0:0-29` | ~52 % |
| 75 % | 23 (22.5 → 23) | 21 | `0:0-41` | ~72 % |
| 100 % | 30 | 30 | `0:0-59` | 100 % |

The 25 % and 75 % rows are the ones that matter: the naive derivation — CU count from a percentage, ascending
first-fit, emit the range — produces `0:0-14` and `0:0-44`, both of which measured **100 % of the card**.

**Conformance table B** — CDNA, measured on a 304 CU / 32 SE / 8 XCC card. "CU/XCC" is what the hardware
registers reported from inside the kernel, so it is occupancy, not intent:

| requested | CUs before align | after align | mask | CU/XCC | measured share |
| --- | --- | --- | --- | --- | --- |
| 1 % | 3 | reject (< 8) | — | — | — |
| 2.63 % | 8 | 8 | `0:0-7` | 1 each | 3.7 % |
| 5 % | 15 | 8 | `0:0-7` | 1 each | 3.7 % |
| 5.26 % | 16 | 16 | `0:0-15` | 2 each | 7.4 % |
| 10.5 % | 32 | 32 | `0:0-31` | 4 each | 13.9 % |
| 50 % | 152 | 152 | `0:0-151` | 19 each | 56.9 % |
| 100 % | 304 | 304 | `0:0-303` | 38 each | 100 % |

The rows that matter are the first and the fourth. A 1 % request must be **refused**: the naive derivation
emits `0:0-2`, which measured **156 of 304 CUs occupied**. And the 5 % row shows the cost of the atom — 15
CUs of budget buy 8 CUs of card, because the seven that do not complete an atom cannot be spent.

**Disjointness is a property of the atoms, not of the bit sets.** Two tenants given the "obviously disjoint"
bit ranges `0:0-3` and `0:4-7` were each measured occupying **156 CUs**, overlapping on 152 of them, while the
allocator's ledger believed it had handed out 4 CUs each. Given XCC-covering masks instead — `0:0-7` and
`0:8-15` — each occupied exactly 8 CUs, they shared nothing, and each measured its solo throughput
(4250 / 4223 GFLOP/s against a 4208 solo baseline). Two tenants deliberately given the *same* mask split it
evenly and reproducibly (2138 / 2140 against the same 4208), which is what makes overlap-on-oversubscription a
usable policy on CDNA as well as on RDNA.

**Self-check.** `tools/rocm-cumask-check` derives a half-card mask for the card it is pointed at and decides
whether the mask took effect. It exists because the platform reports nothing: an unusable mask produces no
error, no log line and no changed return code.

**It must judge by occupancy, not by throughput.** Gate 8 is the reason: `0:0` on a `gfx942` card measured a
perfectly plausible 3.7 % of the card's throughput while 267 of its 304 CUs were reachable by the container,
because the makespan is set by the most-constrained XCC and says nothing about the others. So the probe
launches its own kernel and reads `HW_ID` — and `XCC_ID` where `NUM_XCC > 1` — from inside it, collecting the
set of physical slots its waves actually ran on.

**What it compares that set against is its cardinality, plus its per-XCC partition on CDNA — not its
identity**, and the reason is a mapping nobody has measured. Which physical `(SE, SA, WGP)` triple a given
mask bit lands on is not documented and was not established on hardware; the one axis that *was* read out of
the registers is the XCC, where bit `i` lands on `i mod X`. So the probe asserts what it can defend: that the
number of distinct units occupied equals the number the mask asked for, and on a multi-XCC part that the count
in every XCC does too. That is sufficient, because every fail-open mode measured on either architecture shows
up as a count: a discarded mask occupies the whole card, and an uncovered XCC occupies 38 CUs where the mask
asked for none. A throughput comparison remains as a second, weaker signal for single-XCC parts. Its intended
caller is the detector, which should decline to advertise `.sliced` for a card that fails rather than advertise
a capability the node will not honour.

- Acceptance: the probe returns non-zero for each fail-open construction — Gate 4's three on RDNA, and on
  CDNA a mask that leaves an XCC uncovered — and zero for each valid row of both conformance tables, on real
  hardware; and both tables are carried in the repository as the fixture the handed-off Go will be tested
  against.

#### F5 — Injection contract and the usage surface

**Namespace.** The library's own variables use a `VROCM_` prefix and deliberately do **not** live under
`HIP_`, `HSA_` or `ROCR_`. Those three are ROCm's own namespaces, actively parsed by the runtime and still
growing; a private variable placed there is a future collision. The platform's own variables are used under
their real names because they are the platform's, not ours.

| Variable | Owner | Meaning | Absent in a sliced container |
| --- | --- | --- | --- |
| *(bind mount)* `/etc/ld.so.preload` | loader | activation — one line naming `libvrocm.so`'s container path | no interception at all, silently (see below) |
| `VROCM_DEVICE_MEMORY_LIMIT_<i>` | us | per-card VRAM cap, MiB, `<i>` the **container-local** device index | falls back to the un-indexed figure |
| `VROCM_DEVICE_MEMORY_LIMIT` | us | cap for every card carrying no figure of its own | init error for those cards |
| `VROCM_LEDGER_PATH` | us | the cross-process region's file | init error — see below |
| `LIBVROCM_LOG_LEVEL` | us | `0` silent, `1` denials and errors, `2` also load markers and the counter dump | defaults to `1` |
| `ROCR_VISIBLE_DEVICES` | ROCm | which physical cards the container sees, and **in what order** | — |
| `HSA_CU_MASK` | ROCm | the compute quota, per card | no compute quota |

- **The three device-scoped variables are one tuple and must be emitted together.** `HSA_CU_MASK`'s `GPU_list`
  index and `VROCM_DEVICE_MEMORY_LIMIT_<i>`'s `<i>` are both positions in the **post-`ROCR_VISIBLE_DEVICES`**
  ordering, not physical ordinals — measured: with `ROCR_VISIBLE_DEVICES=1,0`, `0:` addresses physical card 1,
  and `_0` caps physical card 1. Because both index spaces are the same, numbering everything by position in
  the `ROCR_VISIBLE_DEVICES` list makes them agree automatically; changing any one of the three alone
  misaligns the other two.
- **`AMD_VISIBLE_DEVICES` is not part of this contract.** It is read by the container-runtime OCI shim to
  inject device nodes, not by the ROCm user-space runtime — measured, setting it to `1` or to `void` leaves
  `hipGetDeviceCount` unchanged at 2. The existing whole-card AMD allocator injects only this variable and
  relies on the runtime hook; a sliced container needs card-precise ROCr visibility and cannot reuse that.
- **`VROCM_LEDGER_PATH` must be per CONTAINER and is an init error when absent.** The repo already has the
  mechanism — `deviceplugin.PodWorkDir(podUID, containerName)` under `OperatorPodsDir`, which the THead
  backend uses for its ledger and which the device-plugin server garbage-collects. Per container rather than
  per Pod, and the reason is the addressing: a region is indexed by the card's position in
  `ROCR_VISIBLE_DEVICES`, which is container-local, so two containers sharing one region would have their
  index 0 — two *different* physical cards — charge the same slot. Sharing it across a Pod's containers is
  therefore not a looser version of this design, it is a broken one; THead reached the same conclusion for
  the same reason. There is no default path either, so a container the device-plugin did not reach refuses
  every allocation instead of accounting into a location nobody chose.
- **The quota is re-read on attach, never frozen by the region's creator.** A region that caches the first
  writer's limit means a container restarting with a different quota silently keeps the old one — measured as
  a real failure mode: a 2 GiB run against a region created at 4 GiB was still refused only at 4 GiB.
- **`LIBVROCM_LOG_LEVEL` defaults to `1` and the allocator injects `1`, not `0`.** Level 1 here is
  per-*denial*, not per-call: it is the one line that answers "why was my allocation refused". `0` exists for
  a workload that wants absolute quiet. Level `2` is not decoration — the cases decide rows by grepping the
  load marker and the counter dump, so a shim with no way to raise the level would silence most of them. The
  allocator must respect a workload-declared value (`ContainerEnvDeclared`), as the other backends do.
- **Preload failure is silent and fails open, and that is a property of the loader, not of this design.**
  Measured: a missing file and a non-ELF file each produce one `ERROR: ld.so: … ignored.` line on stderr and
  the process runs unconstrained. `LD_AUDIT` behaves identically for the same cases — **it is not the safer
  mechanism it is often assumed to be**, which is one reason it lost the comparison in Gate 7. Under musl the
  mechanism is ignored entirely; that is close to moot here, since the ROCm user space a sliced workload needs
  is glibc-only, but it is why the image-side validation belongs to whoever builds the workload image and not
  to this library, which by definition is not running when it matters.
- **The policy is not a boundary.** Removing the preload restores the full card's memory (measured: 2 GiB cap
  → 15.750 GiB), `env -u HSA_CU_MASK` restores the full card's compute, and `HSA_CU_MASK_SKIP_INIT=1` — which
  appears in no AMD-published environment-variable table — does the latter without removing anything
  (measured: 4128 → 19707 GFLOP/s). A child process started with a cleared environment loses our variables
  (measured: `env -i` → 15.750 GiB); note that `/etc/ld.so.preload` is a **file**, so the library itself
  survives an `env -i` that a `LD_AUDIT`/`LD_PRELOAD` variable would not — a modest and real robustness
  advantage of the file form for multi-process engines that re-exec with a clean environment. The quota
  variables still need `/proc/1/environ` recovery in that case.
- Measured overhead of activation is `+20 ms` on process start (`+8 %`), which is not a concern.

**Usage surface.** The ledger's region is the only machine-readable place a slice's usage exists, so its layout
carries a magic and a version and is documented as a contract with three consumers: the cross-process quota
itself, `tools/rocm-monitor`'s output, and a future metrics scraper. It must carry, per card: the quota in
force, the accounted total, and per-process charges; the per-process breakdown is what lets a dead process's
charge be identified and dropped. **Not through `Devices.Status`** — that is rebuilt wholesale each reconcile
from the Spec plus Pod annotations with no live query, so live usage cannot ride it. The scraper itself is out
of scope.

#### F6 — `gpustack-operator-xbuild-and-verify` AMD backend

- A fourth backend: an `xbuild-amd-rocm` arm in `scripts/build.sh` that stages the tree and calls its own
  `build.sh` inside a ROCm devel image; hardware WARN rows in `scripts/preflight.sh` (`rocm-smi`, `/dev/kfd`,
  the `amdgpu` module); a fourth Cases table and env-knob group in `SKILL.md`; and per-script `allowed-tools`
  entries, since that list has no glob and an unlisted script cannot run.
- Cases, one per gate plus the unit tests:
  (1) build and linkage assertions, **no GPU**; (2) single-card injection and the reported-capacity surface
  across all three property entry points; (3) memory-path completeness including the pool family; (4) CU-mask
  conformance and, for the architecture under test, every one of its fail-open constructions; (5) compute quota
  semantics — barrier-synchronised, saturating, single-tenant ceiling **and** multi-tenant sharing;
  (6) `common/`'s unit tests plus two processes in one container against one quota, and one container across
  two cards with different quotas; (7) lifecycle — `SIGKILL` reclaim and cross-ROCm-version reach.
- **Every case runs against both an RDNA and a CDNA host**, and cases 4 and 5 select which conformance table
  and which fail-open set to assert from the card's `NUM_XCC`. A run against one architecture is a partial
  result: F4's two branches share no arithmetic, so a green suite on one says nothing about the other.
- Case 5 must measure the **single-tenant ceiling**, not only concurrent aggregate throughput, and on a
  multi-XCC card it must also measure **occupancy**. Measured on RDNA, a correctly-partitioned pair, a broken
  partition and no partition at all produce indistinguishable concurrent readings (5061.7 / 5061.7 vs
  5056.2 / 5056.2); the difference appears only when one tenant runs alone. Measured on CDNA, the harder case:
  two tenants sharing 152 CUs and two tenants sharing none report the *same* per-tenant throughput, solo runs
  included, and only the `HW_ID` occupancy readout tells them apart. A suite that checks only throughput
  passes on a card with no isolation whatsoever.
- Acceptance: case 1 passes with no GPU present; every hardware case produces PASS/FAIL rows with captured
  output; and the fail-open constructions are present as **negative** rows, so a case set that stopped
  detecting them fails rather than quietly narrowing.

### Notes / Constraints / Caveats

- C11, no C++, no external dependency. The product links `libc` alone.
- `csrc/amd/rocm-slicing-shim/build.sh` is the only place that knows translation units and flags, mirroring
  `csrc/thead/ppu-slicing-shim/build.sh`; it is silent on success because case 1 decides "compiles clean" on
  empty output.
- The build image is a ROCm devel image chosen for its **glibc**, not its ROCm version — the product is
  ROCm-agnostic, so the only thing the base tag controls is the floor. With the `.symver` pins in F1 the floor
  is held by source rather than by the base tag, so a current image is fine and the assertion is what enforces
  it; the image is `linux/amd64` only either way, because ROCm ships no aarch64 user space, so the `arm64`
  operator image carries no AMD shim and needs the per-`TARGETARCH` stand-in stage idiom already used for
  THead.
- HIP headers are used for type-checking the wrapper signatures and for `offsetof`, and must not create a
  `DT_NEEDED` entry. `hipDeviceProp_t`'s layout is read with `offsetof` at build time rather than hard-coded;
  the measured values (1472 / 288 / 388) are a regression fixture, not a constant — they are identical on
  ROCm 7.2 / `gfx1101` and ROCm 7.2.4 / `gfx942`, so one fixture covers both architectures.
- Timing-based cases must synchronise their start across processes and use a saturating kernel. Both mistakes
  were made during the research and both produced physically impossible numbers.
- **A per-entry coverage case must set the quota *below* one request.** Running each allocation entry with a
  quota larger than the request, freeing between entries, passes whether or not the entry is charged, and was
  the first version of Gate 8's family table — it reported every path as fine. With the quota at 256 MiB and
  each request at 512 MiB the same table separates charged from uncharged on the first row.
- A PyTorch-based compute case must keep warm-up out of the timed window: on this hardware a first
  8192² fp16 GEMM exceeded 400 s of autotuning.

### Boundaries

- **Always:** charge the pool family alongside the classic one; interpose all three reported-capacity entry
  points; resolve through `RTLD_NEXT` **and** `dlopen(RTLD_NOLOAD)`; hold one lock across
  check-allocate-charge; emit `ROCR_VISIBLE_DEVICES`, `HSA_CU_MASK` and `VROCM_DEVICE_MEMORY_LIMIT_<i>` as one
  tuple indexed by position in the ROCr list; read topology through `binding/hsa` or `binding/amdgpu`; keep
  the product's `DT_NEEDED` at `libc.so.6`.
- **Ask first:** raising the `GLIBC_2.4` floor (it costs Ubuntu 20.04 / RHEL 8 workload images); switching to
  `LD_AUDIT` (it is the documented escalation, but it diverges from four backends and constrains `common/`);
  adding any launch-path interception (it puts the library on the hot path and re-opens the compute design);
  changing the ledger's on-disk layout after the first tagged release.
- **Never:** derive shader-engine count from KFD sysfs; treat a reported shader-engine count as per-XCC
  without dividing by `NUM_XCC`; emit a CU mask that splits a WGP pair on RDNA; emit a CU mask that leaves an
  XCC uncovered on CDNA, or clamp an under-sized compute request into one instead of rejecting it; judge a
  mask by throughput alone on multi-XCC hardware; put our variables in the `HIP_`/`HSA_`/`ROCR_` namespaces;
  call `pthread_*` or `sem_*` anywhere in the product; return a fabricated HIP status when a symbol cannot be
  resolved; default the ledger path to a shared host location; describe this mechanism as a security
  boundary.

### Risks and Mitigations

- **A mis-derived mask silently hands over the whole card** → the conformance table is a fixture, the
  self-check probe is shipped, and the negative constructions are cases rather than comments. The detector
  should decline `.sliced` for a card whose probe fails.
- **The interception table drifts behind ROCm** — a future release adds an allocation entry point and the
  quota quietly stops being a quota → the manifest is regenerated against the build image's own
  `libamdhip64` and diffed in CI; case 3 exercises each family through its own entry rather than assuming one
  funnels into another.
- **`hipDeviceProp_t` grows or reorders between ROCm releases** → the offset is taken with `offsetof` at build
  time and the measured values are a regression fixture; a struct that changed shows up as a case-2 failure,
  not as a wrong number in production.
- **A fixed-capacity allocation table overflows and leaks charges** → insert failure is fail-closed, and case 6
  fills the quota with many allocations, frees them all, and re-requests the whole quota; it is admitted only
  if every refund landed.
- **A shared ledger cross-charges whoever shares it** → the path is per container, comes from
  `PodWorkDir(podUID, containerName)`, and is an init error when absent rather than defaulting. Per container
  and not merely per Pod: the region is addressed by a container-local card index, so two containers of one
  Pod sharing a region would charge two different cards into one slot.
- **A future ROCm release starts self-calling an allocating entry point**, which a preload cannot decline and
  which would double-charge or recurse → the caller-origin diagnostic makes it a visible line naming
  `libamdhip64` rather than a silent miscount; the in-process lock is re-entrancy-counted so a nested call
  cannot deadlock; and `LD_AUDIT` is the documented escalation, confined to `hip/`. Case 3 asserts the
  baseline — zero self-calls in the allocation family — so a change shows up as a failing row.
- **The compute quota is not work-conserving** — a disjointly-packed idle tenant's WGPs cannot be used by
  anyone → documented, and the overlap-on-oversubscription policy is what recovers utilisation when it matters.
- **A CU mask carries no memory-bandwidth isolation, and Gate 8 quantified it** — a bandwidth-saturating
  tenant on a disjoint half costs its neighbour a further 25 %, and half the CUs already reach 97.7 % of the
  card's bandwidth → this is not mitigable by the derivation, so it is a **documentation and product**
  obligation instead: `.sliced` must be described as a compute *ceiling*, never as a compute QoS or as
  isolation, in the same place the request API's `.sliced.cores-*` dimension is explained. A deployment that
  needs bandwidth isolation needs hardware partitioning, not this.
- **A CDNA mask that misses an XCC passes every throughput check while leaking most of the card** → the
  self-check probe must assert **occupancy**, not throughput: it reads `HW_ID`/`XCC_ID` from inside its own
  kernel and fails the card unless the CUs it actually ran on match the CUs the mask asked for. Gate 8
  measured `0:0` at a healthy-looking 3.7 % while occupying 267 of 304 CUs, so a throughput-only probe would
  have passed it. The negative constructions are cases, and the derivation rejects any bit set that does not
  cover every XCC before it is ever emitted.
- **Only one CDNA SKU and one partition mode were measured** — `gfx942` in SPX/NPS1 under SR-IOV, single card.
  CPX/DPX compute partitioning splits one physical card into several agents with a different `NUM_XCC` per
  agent, and multi-card CDNA hosts were not available → the derivation is parameterised on the queried
  `NUM_XCC` rather than on a per-SKU constant, so a partitioned agent reporting `NUM_XCC = 1` should fall
  through to the single-XCC path correctly; the occupancy self-check is the runtime gate that stops an
  unverified partition mode from advertising a capability. Carried as an Open Question with a named
  experiment.

## Design Details

### Commands

**Environment.** Everything C in this spec builds and runs **on an AMD host**, not locally: the product needs
a Linux glibc toolchain, `hip/` needs HIP headers, and every case past the first needs a real card. The host is
reached over SSH through the skill's own `XB_MODE=ssh` transport, so no command below assumes a local ROCm
install and none of them require a 4.8 GB devel image on the developer's machine. Only the Go targets — which
this spec does not touch — run locally, and they are listed as a regression guard rather than as work.

**One required host, one opportunistic.** The **RDNA host is the baseline** every case must pass against; it
is the hardware this project owns. A **CDNA host is run when one is available** — the CDNA host used for
Gate 8 is a rented single-card instance, so it cannot be assumed present and no task blocks on it. What makes
that acceptable is that the two branches of F4's derivation are exercised in different places: the CDNA
branch's arithmetic is unit-tested against the checked-in conformance table B, which Gate 8 measured, and
`rocm-cumask-check` re-verifies it on the node at run time before a card is ever advertised. Every case is
written to read the branch it is on from `NUM_XCC` rather than from a flag, so pointing it at a CDNA host is
the only thing needed when one exists. When a rented CDNA host is used, note that it is behind a proxy
offering only an interactive shell — no `exec`, no `scp` — so `XB_SSH` must not be assumed to support command
execution; where it does not, the case harness pipes its script over stdin. A run that covered only one
architecture is a partial result and says so in its summary.

```bash
# ---- the ONE place that knows how to compile this tree (runs INSIDE a container) ----
csrc/amd/rocm-slicing-shim/build.sh lib     # the product: libvrocm.so
csrc/amd/rocm-slicing-shim/build.sh tool    # tools/: rocm-monitor, rocm-cumask-check
csrc/amd/rocm-slicing-shim/build.sh test    # testing/: the gate binaries
csrc/amd/rocm-slicing-shim/build.sh unit    # common/'s unit tests -- no ROCm, no device, runs anywhere
csrc/amd/rocm-slicing-shim/build.sh check   # the four linkage assertions case 1 re-runs
csrc/amd/rocm-slicing-shim/build.sh list <name>   # the translation units behind one artifact
# Silent on success -- case 1 decides "compiles clean" on empty output, not on the exit status.
# Env: OUT (artifact dir) - CC (default gcc) - V=1 (trace).

# ---- verification entry point: stages the tree onto the target, compiles it in a ROCm devel image ----
# Follows the in-repo-source shape (stage + run), not the buildx shape the vendored-source backends use.
SKILL=.claude/skills/gpustack-operator-xbuild-and-verify
XB_MODE=ssh XB_SSH=<user>@<rdna-host> bash ${SKILL}/scripts/build.sh xbuild-amd-rocm
XB_MODE=ssh XB_SSH=<user>@<cdna-host> bash ${SKILL}/scripts/build.sh xbuild-amd-rocm

# ---- preflight and the cases ----
# The host needs /dev/kfd, /dev/dri and the amdgpu module; it does NOT need docker if nerdctl is
# present (XB_CTR resolves either). A rented CDNA instance is itself a container: it has neither, so
# the arm must fall back to compiling in place when XB_CTR resolves nothing and ROCm is already on the
# host. Case 1 needs no card; cases 2-7 do. Run the whole set against BOTH hosts.
XB_MODE=ssh XB_SSH=<user>@<rdna-host> bash ${SKILL}/scripts/preflight.sh
XB_MODE=ssh XB_SSH=<user>@<rdna-host> bash ${SKILL}/cases/amd-case-1.sh   # ... through amd-case-7.sh
XB_MODE=ssh XB_SSH=<user>@<cdna-host> bash ${SKILL}/cases/amd-case-1.sh   # ... through amd-case-7.sh

# ---- regression: untouched by this spec, run to prove it ----
make lint          # whole-module golangci-lint --fix; a cold cache needs a long timeout
make test
bash -n csrc/amd/rocm-slicing-shim/build.sh ${SKILL}/cases/amd-case-*.sh
# No shell linter is wired into CI -- hack/lint.sh covers Go and the Helm chart only -- so `bash -n`
# plus each case's own FAILS= contract is the gate the repo actually enforces. shellcheck -x is still
# worth running by hand.
```

### Project Structure

This spec's deltas — note `.claude/skills/` **is** version-controlled (`.gitignore` re-includes it after
`.claude/*`), so those are tracked changes like any other:

```
csrc/amd/rocm-slicing-shim/            # the libvrocm.so source tree -- PRODUCT code at this level
├── README.md                          # how to build, and how to run a slice by hand
├── build.sh                           # lib | tool | test | unit | check | list; silent on success
├── common/                            # no hip*/hsa* type may appear here, which is what makes it
│   ├── vrocm.h                        #     testable with no ROCm and no device
│   ├── vrocm_log.{h,c}                #     the verbosity level
│   ├── vrocm_quota.{h,c}              #     env parsing, the load-time report, the usable/unusable latch
│   ├── vrocm_ledger.{h,c}             #     the versioned region, the per-card fcntl lock, charges,
│   │                                  #     the process-local key map, the liveness sweep
│   └── vrocm_test.c                   #     unit tests -- run by case 6, no device
├── hip/                               # the interposed entry points and the resolver
│   ├── hip_resolve.{h,c}              #     RTLD_NEXT -> dlopen(RTLD_NOLOAD) -> abort; never a fabricated
│   │                                  #     return code. The caller-origin diagnostic lives here too.
│   ├── hip_table.{h,c}                #     the interposed-name table and the per-entry counters
│   ├── hip_mem.c                      #     the classic allocating/freeing family: charge / refund
│   ├── hip_pool.c                     #     the stream-ordered and pool family -- Gate 2's finding
│   └── hip_query.c                    #     hipMemGetInfo + all three reported-capacity entry points
├── tools/                             # preloaded into nothing -- they read
│   ├── rocm_monitor.c                 #     -> rocm-monitor: quota and usage per card, from the region
│   └── rocm_cumask_check.c            #     -> rocm-cumask-check: F4's self-check, the detector's gate
└── testing/                           # gate-only artifacts, never shipped in the library
    ├── hip_mem_paths.c                #     Gate 2's workload half -- one allocation family per invocation
    ├── hip_props_probe.c              #     Gate 3 -- all three property entry points plus hipDeviceTotalMem
    ├── cumask_soak.c                  #     Gates 4/5 -- ILP-saturating kernel, file-barrier start, N tenants
    └── ledger_lifecycle.c             #     Gate 6 -- hold, SIGKILL, re-acquire

.claude/skills/gpustack-operator-xbuild-and-verify/
├── scripts/build.sh                   # + an xbuild-amd-rocm arm calling the tree's own build.sh
├── scripts/preflight.sh               # + three AMD hardware WARN rows
├── cases/amd-case-{1..7}.sh           # NEW
├── references/amd-*.md                # NEW, + an AMD section in troubleshooting.md
├── references/amd-hip-symbol-manifest.md   # NEW: F3's symbol surface + policy, with the image digest
└── SKILL.md                           # + fourth Cases table, AMD env knobs, per-case allowed-tools

specs/2026-08-06-amd-gpu-slicing-shim.md
```

Handed off — listed so the boundary is explicit:

```
pack/gpustack-operator/Dockerfile           # + ONE xbuild-amd-rocm stage (no per-ROCm-major fan-out:
                                            #   the product is version-agnostic), + a COPY to
                                            #   ${GPUSTACK_LIB_DIR}/amd/, + an install -D for the AMD
                                            #   ld.so.preload beside the existing ones, + the arm64
                                            #   stand-in stage. No ARG for an upstream commit: the
                                            #   source is in-repo under csrc/.
pack/gpustack-operator/external/amd/build-libvrocm.sh
pack/gpustack-operator/rootfs/etc/gpustack/lib/amd/ld.so.preload  # one line; a contract with the
                                            #   allocator's in-container mount path constant
pkg/devicemanager/detector/amd/device.go    # + Status.LogicalSliced (Count, CoresPercentageOvercommit=true)
                                            #   at :206, + the device.SetGroupSlicedDetails(grpList) call
                                            #   it lacks at :224, + the F4 self-check gate
pkg/devicemanager/allocator/amd/deviceplugin.go  # + Sliced server behind !opts.NoSliced at :29,
                                            #   + un-discard the pod/ctr params at :98-99, + the injection
                                            #   branch emitting the F5 tuple
pkg/devicemanager/allocator/amd/cumask.go   # F4's derivation -- the two NUM_XCC branches (RDNA: WGP pairs
                                            #   + shader-engine alignment; CDNA: NUM_XCC-sized atoms with
                                            #   mandatory XCC coverage and rejection below one atom), and
                                            #   the disjoint-then-overlap policy
README.md                                   # the accelerator matrix's AMD .sliced column
docs/architecture/discovery.md              # mechanism table, the preload prose (which must grow an
                                            #   AMD row), and "where the preload libraries come from"
```

### Code Style

C sources follow `csrc/thead/ppu-slicing-shim/`: C11, four-space indent, one responsibility per translation
unit, and a comment that states *why* rather than *what*. The one convention this tree adds is that the two
non-obvious preload rules are restated at their site, because both were learned the expensive way:

```c
/* Two lookups, and never a fabricated return code.
 *
 * RTLD_NEXT finds nothing when the framework dlopen()s libamdhip64 instead of
 * linking it -- PyTorch does exactly that -- so the handle lookup is not a
 * belt-and-braces extra, it is the path real workloads take. And a resolve
 * miss must abort rather than return: the natural placeholder 1 IS
 * hipErrorInvalidValue, so returning it turns "we could not find the symbol"
 * into "the runtime rejected your arguments", which is a false trail that
 * costs an afternoon. */
static void *resolve(const char *name) {
    void *p = dlsym(RTLD_NEXT, name);
    if (!p) {
        static void *h;
        if (!h) h = dlopen("libamdhip64.so", RTLD_NOLOAD | RTLD_LAZY);
        if (h) p = dlsym(h, name);
    }
    if (!p) {
        vrocm_logf(VROCM_ERR, "cannot resolve %s", name);
        abort();
    }
    return p;
}

/* A preload cannot decline to fire on a runtime-internal call, so make one
 * visible instead of guessing it away. Measured today: only hipMemGetInfo is
 * ever called by libamdhip64 itself, and that wrapper is idempotent. A line
 * naming libamdhip64 under an allocating entry is the signal that this
 * assumption has expired -- see the escalation path in Risks. */
#define VROCM_TRACE_CALLER(fn)                                                 \
    do {                                                                       \
        if (vrocm_level() >= VROCM_DEBUG) vrocm_trace_caller(                  \
            (fn), __builtin_return_address(0));                                \
    } while (0)
```

`GLIBC_2.4` is held by avoiding `pthread_*` and `sem_*` everywhere, including in `common/`; in-process
exclusion is a re-entrancy-counted GCC atomic spinlock and cross-process exclusion is an `fcntl` record lock.

### Implementation Plan

Ten tasks. T1 is the foundation and the only task nothing else can start without; after it, **five tasks
(T2, T3, T4, T5, T6) are unblocked with disjoint `Owns:`** and build concurrently. The cases land once both
the artifacts and the entry point exist, and T10 folds the measured figures back into this file.

Paths in `Owns:` are relative to the repository root; `SHIM` abbreviates `csrc/amd/rocm-slicing-shim` and
`SKILL` abbreviates `.claude/skills/gpustack-operator-xbuild-and-verify`. Every `Verify` runs on the AMD host
through `XB_MODE=ssh` (see Commands) unless it says otherwise.

Three ordering decisions are deliberate, because each one buys parallelism that a naive cut would lose:

- **T1 writes `build.sh`'s complete artifact table up front**, including recipes for artifacts that do not
  exist yet. The alternative — each task appending its own recipe — would put five tasks on one file and
  serialise the whole fan-out. The cost is that `build.sh lib` fails until T2 lands, so T1's `Verify` is
  scoped to `unit`, `list` and the mutation checks.
- **`hip/` is ONE task, not two.** An earlier cut split the resolver and the reported-capacity family from the
  allocating families, on the reasoning that a family editing another's table is a write conflict wearing a
  dependency's clothes. That reasoning holds only for tasks that run at the same time, and these two never
  could: the second was blocked by the first, nothing else depended on the first alone, and the split bought
  no parallelism at all. What it did buy was a first task with **nothing to assert against** — `libvrocm.so`
  does not link until every one of its translation units exists, so the `.symver` pins would have shipped one
  task before the assertion that proves they work. The families still live in their own translation units;
  what changed is that one task writes them.
- **T4 does not depend on `hip/` at all.** The mask self-check needs topology and a micro-benchmark, not the
  ledger and not the interposer, so it runs beside T2 rather than after it.

- [x] **T1 · Tree skeleton, `build.sh`, and `common/`**
      Blocked by: None
      Owns: `SHIM/build.sh`, `SHIM/README.md`, `SHIM/common/**`
      Gate: review
      Acceptance: `build.sh` implements `lib | tool | test | unit | check | list`, declares the translation
      units for **every** artifact this spec names (including the ones T2–T5 will fill in), is silent on
      success, and honours `OUT` / `CC` / `V=1`. `common/` lands complete and contains **no** `hip*`/`hsa*`
      type and **no** `pthread_*`/`sem_*` call — that is what lets it be tested with neither ROCm nor a
      device: `vrocm.h`, `vrocm_log.{h,c}` (the three levels), `vrocm_quota.{h,c}` (per-card and un-indexed
      parsing, the load-time report, the unusable latch), `vrocm_ledger.{h,c}` (the versioned region with its
      magic and header, the per-card `fcntl` record lock at a version-frozen offset, the re-entrancy-counted
      in-process spinlock, charge/refund, the process-local key map, the liveness sweep) and `vrocm_test.c`.
      Four ledger properties are behavioural, not incidental, and each gets a named test: the quota is
      **re-read on attach** rather than frozen by the region's creator; check-allocate-charge happens under
      **one** lock acquisition; a tracker insert that cannot be satisfied is **fail-closed**; and a dead
      process's charge is swept rather than held forever.
      Verify: `build.sh unit` inside a plain `ubuntu:22.04` container → every named case passes, with no ROCm
      image and no device node; `build.sh list <name>` prints a non-empty TU list for every declared artifact.

- [x] **T2 · `hip/` — the interposer, end to end**
      Blocked by: T1
      Owns: `SHIM/hip/**`
      Gate: review
      Acceptance, in three parts that together make one linkable artifact.

      *The resolver and the table.* One resolver serves every wrapper — `dlsym(RTLD_NEXT, …)`, then
      `dlopen("libamdhip64.so", RTLD_NOLOAD | RTLD_LAZY)` and `dlsym` on that handle, then **abort with the
      symbol name**. It must never fabricate a status: `1` is `hipErrorInvalidValue`, so returning it on a
      resolve miss disguises "symbol not found" as "the runtime rejected your arguments". `hip_table` provides
      the registration mechanism, the per-entry counters and the level-2 caller-origin diagnostic (`dladdr` on
      `__builtin_return_address(0)`, first N firings per entry). `hip_resolve.c` carries **three** `.symver`
      pins — `dlopen`, `dlsym` and `dladdr`, all `@GLIBC_2.2.5`; without them those symbols alone bind at
      `GLIBC_2.34` on any build host newer than that and the F1 floor assertion fails. `dladdr` is the one to
      miss, because it arrives with the diagnostic rather than with the resolver.

      *The reported-capacity family.* `hip_query.c` covers **all three** paths — `hipMemGetInfo`,
      `hipGetDevicePropertiesR0600` and `hipDeviceTotalMem` — plus the plain `hipGetDeviceProperties` for
      callers built against pre-6.0 headers. `totalGlobalMem`'s offset comes from `offsetof` at build time;
      the measured figures (struct 1472, `totalGlobalMem` 288, `multiProcessorCount` 388 — identical on
      `gfx1101`/ROCm 7.2 and `gfx942`/ROCm 7.2.4) are checked in as a regression fixture, not hard-coded into
      the wrapper.

      *The allocating families.* The classic family (`hipMalloc`, `hipMallocManaged`, `hipMallocPitch`,
      `hipExtMallocWithFlags`, `hipMallocArray`, `hipMalloc3DArray`) and the **stream-ordered/pool family**
      (`hipMallocAsync`, `hipFreeAsync`, `hipMallocFromPoolAsync`) are both charged against the per-card
      figure through `vrocm_ledger_admit()`, and the freeing entries refund exactly once per pointer. Every
      name in that list is a door somebody measured open, not a precaution: the pool family lets a 2 GiB quota
      hold 12 GiB, and `hipMallocManaged`, `hipExtMallocWithFlags` and `hipMallocPitch` each satisfied a
      512 MiB request under a 256 MiB quota when only `hipMalloc` and the pool family were wrapped.
      `hipMallocArray` returns "operation not supported" on `gfx942`, so its coverage can only be proven on
      RDNA. Host-memory entries are counted and never charged — pinned host pages are not device VRAM — and an
      **imported** pool pointer is deliberately not recorded, because it maps memory another process already
      paid for and crediting this container's free for it would refund memory that was never taken. The
      pitched entries admit on the caller's width and reconcile to `stride × height` under the same lock,
      reporting rather than refusing a stride that overruns, since freeing a successful allocation behind the
      caller's back would break a working workload over padding it never asked for.

      **Each family still registers its own entries from its own translation unit** — `hip_query.c`,
      `hip_mem.c`, `hip_pool.c` — even though one task now writes all three. The reason is no longer write
      contention but blast radius: a table that listed every entry in one file would make every family's
      change a diff against every other family's.
      Verify: `build.sh lib` clean and `build.sh check` green (exports only intercepted HIP names, `NEEDED` is
      `libc.so.6` alone, no `GLIBC_` above `GLIBC_2.4`, zero undefined `hip*`/`hsa*`) **on a glibc ≥ 2.35 build
      image**, which is what makes the pins load-bearing rather than decorative. Then on the RDNA host: under a
      4 GiB quota a probe reports 4.000 GiB through `hipMemGetInfo` **and** `hipDeviceProp_t.totalGlobalMem`
      **and** `hipDeviceTotalMem`; and under a 2 GiB quota the per-entry probe reports `hipMalloc` stopping at
      2.000 GiB **and** `hipMallocFromPoolAsync` returning `hipErrorOutOfMemory` rather than the extra
      10.000 GiB it takes unwrapped, with total device memory held equal to the quota.

- [x] **T3 · `tools/rocm-monitor`**
      Blocked by: T1
      Owns: `SHIM/tools/rocm_monitor.c`
      Acceptance: prints quota and accounted usage per card by mapping the region **read-only** and parsing it
      from the layout contract alone. It must link none of `common/`'s ledger code: that code maps lazily and
      would *create* a region, and its other entries take the card's lock — a reader must do neither. It needs
      neither ROCm nor a device.
      Verify: run against a region written by T1's unit-test fixture → the figures match; `nm -D` and
      `build.sh list rocm-monitor` both confirm no `vrocm_ledger` object is linked in; running it when no
      region exists prints a diagnostic and creates nothing (checked with `ls` before and after).

- [x] **T4 · `tools/rocm-cumask-check` and the mask conformance fixture**
      Blocked by: T1
      Owns: `SHIM/tools/rocm_cumask_check.c`, `SKILL/references/amd-cumask-conformance.md`
      Gate: review
      Acceptance: the probe reads the card's topology through the HSA agent-info API — `COMPUTE_UNIT_COUNT`,
      `NUM_SHADER_ENGINES`, `NUM_SHADER_ARRAYS_PER_SE`, `NUM_XCC` — and **never** from KFD sysfs, remembering
      that the reported shader-engine count is device-wide and already carries the XCC multiplier. It branches
      on `NUM_XCC`, derives a half-card mask per the matching F4 branch, and **decides by occupancy, not by
      throughput**: it launches its own kernel, reads the wave's own hardware identity from inside it, and
      exits non-zero unless the units it ran on are the ones the mask asked for. A throughput comparison is
      kept as a secondary signal only — Gate 8 measured a mask that passes a throughput check while leaking
      267 of 304 CUs.

      **The register and the unit both differ by architecture, and each matches that architecture's
      allocation atom.** On CDNA the wave reports `HW_ID` (`CU_ID[11:8]`, `SH_ID[12]`, `SE_ID[15:13]`) plus
      `XCC_ID`, and the unit compared is the **CU**. On RDNA the register is `HW_ID1`
      (`SIMD_ID[9:8]`, `WGP_ID[13:10]`, `SA_ID[16]`, `SE_ID[20:18]`) and the unit compared is the **WGP** —
      measured, `SIMD_ID` only ever reports 0 or 1 there, so the two CUs of a WGP are not distinguishable from
      inside a wave and a CU-level comparison would be counting something the hardware does not expose. That
      is not a limitation in practice: the WGP is exactly RDNA's allocation atom, so each architecture is
      compared in the unit it allocates in. Confirmed against the mask on real hardware — unmasked reports 30
      WGPs, `0:0-29` reports 15, `0:0-13` reports 7, `0:0-1` reports 1.

      Both F4 conformance tables are checked in as the fixture the handed-off Go will be tested against, with
      each architecture's fail-open constructions as negative rows.
      Verify: on the RDNA host, topology reads CU=60, SE=3, SA/SE=2, XCC=1; exit 0 for each table-A row and
      **non-zero** for each of `0:0-14`, a `ROC_GLOBAL_CU_MASK` whose bits all sit at or above the WGP count,
      and a `GPU-<hex>` `GPU_list`. On the CDNA host, topology reads CU=304, SE=32, SA/SE=1, XCC=8; exit 0 for
      each table-B row and **non-zero** for `0:0`, `0:0-3`, `0:0,8,16,24`, `0:304-400` and a `GPU-<hex>`
      `GPU_list` — the first three of which a throughput-only probe would pass.

- [x] **T5 · `testing/` gate programs**
      Blocked by: T1
      Owns: `SHIM/testing/**`
      Acceptance: four programs, seeded from the PoC artifacts this spec's gates were measured with.
      `hip_mem_paths` exercises one allocation family per invocation so a case can name which path crossed.
      `hip_props_probe` reads all three reported-capacity entry points plus `hipDeviceTotalMem` and prints
      which symbol actually bound. `cumask_soak` carries the **file barrier**, the **ILP-saturating**
      kernel and the **`HW_ID` occupancy readout** — all three are load-bearing rather than stylistic: without
      the barrier N tenants report an aggregate above the card's peak; a latency-bound kernel under-fills a
      small partition and inflates every overlap reading; and without the occupancy readout a mask that leaks
      most of a multi-XCC card reads as a healthy slice. It therefore reports GFLOP/s **and** the set of
      physical CUs its own waves occupied, per tenant. `ledger_lifecycle` holds a charge, takes `SIGKILL`, and
      re-acquires.
      Verify: `build.sh test` clean; `cumask_soak` self-checks that two same-mask tenants aggregate to
      **one** tenant's solo figure (the saturation check) and that its barrier released both within one
      window; on the RDNA host the unmasked full-card figure is reproducible across three runs and the
      unmasked occupancy readout equals the card's CU count (60 on RDNA; 304 on CDNA, 38 per XCC).

- [ ] **T6 · Verify-skill wiring: the `xbuild-amd-rocm` arm, preflight, and `SKILL.md`**
      Blocked by: T1
      Owns: `SKILL/scripts/build.sh`, `SKILL/scripts/preflight.sh`, `SKILL/SKILL.md`
      Gate: review
      Acceptance: an `xbuild-amd-rocm` arm that **stages the tree and compiles it inside a ROCm devel image**,
      following the in-repo-source shape rather than the buildx path the vendored-source backends use — the
      source is in this repo, so there is no upstream commit to pin and nothing to fetch. It must also survive
      a target that **is already a container**: a rented CDNA instance has ROCm on the host filesystem and
      neither docker nor nerdctl, so when `XB_CTR` resolves nothing and `/opt/rocm` is present the arm compiles
      in place rather than failing. The same target may expose only an interactive SSH shell — no `exec`, no
      `scp` — so the transport must not assume `ssh <host> <command>` works. `preflight.sh` gains three AMD
      hardware WARN rows (`rocm-smi` probed by `PATH` and by `/opt/rocm/bin`, the `/dev/kfd` and
      `/dev/dri/renderD*` nodes, the `amdgpu` module) plus a fourth reporting `NUM_XCC`, since that value
      selects which conformance table the compute cases assert; none of them is a new FAIL condition.
      `SKILL.md` gains a fourth Cases table, an AMD env-knob group, and a **per-script** `allowed-tools` entry
      for each of the seven case scripts — that list has no glob, so an unlisted script cannot run.
      Verify: `bash -n` on all three; `preflight.sh` on the RDNA host → `FAILS=0` with four AMD rows reporting
      `NUM_XCC=1`, and the same on a CDNA host reporting `NUM_XCC=8` when one is available; `preflight.sh`
      locally → the AMD rows WARN rather than FAIL; `build.sh xbuild-amd-rocm` produces `libvrocm.so` and both
      tools on the RDNA host, and on a CDNA host when one is available, including one with no container
      runtime.

- [ ] **T7 · Cases 1–3 and the symbol manifest**
      Blocked by: T2, T5, T6
      Owns: `SKILL/cases/amd-case-{1,2,3}.sh`, `SKILL/references/amd-hip-symbol-manifest.md`
      Gate: review
      Acceptance: **case 1 needs no GPU** and asserts the four linkage properties per artifact, using
      `readelf -W --dyn-syms` to require `GLOBAL DEFAULT` with a non-`UND` section — never `nm -D | grep`,
      which also matches an *imported* symbol and would pass for any library that merely calls it. It records
      each artifact's staged path and `sha256` so the later cases consume exactly what it produced. Case 2
      injects into a single card and reads all three reported-capacity entry points, with a control arm that
      interposes only `hipMemGetInfo` and must show the physical figure through `totalGlobalMem` — that
      control is what proves the R0600 finding rather than coincidence. Case 3 walks each allocation family
      through its own entry, asserts the pool family is charged, and asserts the **caller-origin baseline**:
      zero self-calls in the allocation family, `hipMemGetInfo` the only symbol `libamdhip64` calls into. The
      manifest re-establishes every interposed name against the `libamdhip64` **inside the build image**, with
      its version tag, its substitution policy, the image digest and the command that regenerates it.
      Verify: `amd-case-1.sh` on a host with no card → `FAILS=0`; cases 2 and 3 on the AMD host → `FAILS=0`
      with captured output; re-running the manifest's recorded command reproduces its generated block byte
      for byte.

- [ ] **T8 · Cases 4–5 — mask conformance and compute semantics**
      Blocked by: T4, T6
      Owns: `SKILL/cases/amd-case-{4,5}.sh`
      Acceptance: case 4 selects its conformance table from the card's `NUM_XCC` and drives
      `rocm-cumask-check` across every row of it and every fail-open construction for that architecture, as
      PASS and **negative** rows respectively, so a case set that stopped detecting the fail-open modes fails
      rather than quietly narrowing. Case 5 measures the **single-tenant ceiling** as well as concurrent
      aggregate — measured, a correct partition, a broken partition and no partition at all produce
      indistinguishable concurrent readings, and the difference appears only when one tenant runs alone — plus
      the disjoint, fully-overlapping and mixed-capped rows. On a multi-XCC card it must additionally assert
      **occupancy** per tenant, not throughput alone: Gate 8's naive-bit-split row has both tenants reading a
      healthy solo-equivalent figure while sharing 152 CUs. Every timed row uses the barrier and the saturating
      kernel from T5; any PyTorch arm keeps warm-up outside the timed window.
      Verify: both on the RDNA host → `FAILS=0`; case 5's single-tenant half-card row lands near 50 % of the
      unmasked figure and its three-tenant same-mask row is reproducible across repeats. On a CDNA host, when
      one is available, the XCC-covering disjoint pair reports each tenant occupying exactly its own CUs while
      the naive bit-split pair is **rejected** by case 4 before case 5 would ever measure it.

- [ ] **T9 · Cases 6–7 — unit plus multi-tenant, and lifecycle plus version reach**
      Blocked by: T3, T7
      Owns: `SKILL/cases/amd-case-{6,7}.sh`
      Acceptance: case 6 runs `common/`'s unit tests, then two processes in one container against one quota,
      then **one container across two cards carrying different quotas** — the last is what gives per-card
      keying a behavioural test, without which a shim charging one container-wide figure would pass every
      other row. It also fills the quota with several allocations, frees them all, and re-requests the whole
      quota, admitted only if every refund landed. Case 7 covers `SIGKILL` reclaim and cross-version reach:
      the same artifact is exercised inside both a ROCm 7.x and a ROCm 6.x container, since the product links
      no ROCm object and one build is claimed to serve every version.
      Verify: both on the AMD host → `FAILS=0`, with the two-card row showing each card held to its own
      figure and the 6.x arm enforcing the same quota as the 7.x arm.

- [ ] **T10 · Fold the measured figures back into this spec**
      Blocked by: T7, T8, T9
      Owns: `specs/2026-08-06-amd-gpu-slicing-shim.md`, `SKILL/references/amd-*.md`,
      `SKILL/references/troubleshooting.md`
      Acceptance: every projected figure in F2–F6 is replaced by what the delivered cases measured, or is
      marked as still projected; the Open Questions are re-answered against what shipped; and the two
      references the cases lean on (`amd-hip-symbol-manifest.md`, `amd-cumask-conformance.md`) are joined by an
      AMD section in `troubleshooting.md` covering the failure modes this work actually hit — a preload that
      silently did nothing, an `RTLD_NEXT` miss under a `dlopen`ing framework, a timing run without a barrier,
      a `GLIBC_2.34` ceiling traced to the resolver's own `dlopen`/`dlsym`, and a multi-XCC mask that measured
      as a working slice while occupying most of the card.
      Verify: `bash -n` clean; the spec reads top-to-bottom with no claim the cases did not establish; every
      `Status:`-relevant field is current.

**Checkpoints.** After T1 the tree builds and its unit tests pass with no ROCm and no device. After T2 and T6
the library is complete and has an entry point, so every later task is verification rather than construction.
After T9 all seven cases are green and T10 is bookkeeping.

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- **None for Go.** This spec adds no Go source, so `make lint` and `make test` are run as an untouched-baseline
  regression guard rather than as work.
- **A C unit-test harness does not exist in this repository yet and T1 creates one.** `csrc/` has one other
  tree and its tests are a single self-contained program invoked by its own `build.sh unit`; this tree follows
  that convention rather than introducing a framework. The tracked figure is therefore the named-case list
  below, not a percentage — **no coverage instrumentation is wired for C here**, and wiring `gcov` is out of
  scope for a spec that ships a preload library.
- **No shell linter runs in CI** (`hack/lint.sh` covers Go and the Helm chart only), so `bash -n` on every new
  script plus each case's own `FAILS=` contract is the enforced gate. `shellcheck -x` is expected to be run by
  hand, since these scripts carry `# shellcheck disable=` directives and so presume a reader that honours them.

#### Unit tests

`SHIM/common/` via `build.sh unit` — no ROCm, no device, runs in a plain `ubuntu:22.04` container. Named cases,
grouped by the property each defends:

- `vrocm_quota`: per-card figure wins over the un-indexed one wherever it is **set**; a set-but-malformed
  figure makes that card unusable rather than falling through to the level above; the unusable latch refuses
  every allocation for that card; a container with only the un-indexed figure is a complete configuration.
- `vrocm_ledger` — region: magic and version are rejected when foreign; the per-card lock byte sits at a
  version-frozen offset, so two builds speaking different versions still lock the same byte for the same card;
  the quota is **re-read on attach** and a region created at 4 GiB does not pin a later 2 GiB run.
- `vrocm_ledger` — accounting: charge and refund round-trip; check-allocate-charge happens under **one** lock
  acquisition (asserted by a hook that fails if the lock is released between the check and the charge); a
  tracker insert that cannot be satisfied is **fail-closed** and never silently drops a charge; a dead
  process's charge is swept and the card's total re-derived from live slots.
- `vrocm_ledger` — locking: the in-process spinlock is re-entrancy-counted, so a nested interposed call cannot
  deadlock; the process-local key map is not shared, so one process's free cannot refund another's allocation.
- `vrocm_log`: level 0 is silent, level 1 emits denials and errors only, level 2 adds load markers and the
  counter dump — the cases grep the last two, so their strings must exist in the binary at every level.
- **Mutation checks.** Each of the four behavioural ledger properties is re-run against a deliberately broken
  build — quota frozen at creation, lock released between check and charge, insert failure ignored, sweep
  removed — and each must fail a **named** row. A test that passes against the broken build is decoration and
  is rewritten, not kept.

Per-package targets:

- `csrc/amd/rocm-slicing-shim/common`: `2026-08-07` - named cases above; no coverage tooling wired (see
  Prerequisite testing updates)
- Go packages: `None` — this spec adds no Go

#### Integration tests

The seven numbered cases under `SKILL/cases/`, run through `XB_MODE=ssh`. The **RDNA host is the required
target**; a CDNA host (`NUM_XCC = 8`) is run **when one is available** and no task blocks on it — see
Commands. Concrete test names are the case scripts themselves; each emits PASS/FAIL rows with captured output
and a `FAILS=` total. Cases 1-3, 6 and 7 assert the same rows on either host; cases 4 and 5 select their table
from `NUM_XCC`, so a run against one architecture leaves the other branch of F4 covered only by its unit tests
and by `rocm-cumask-check` at run time, and the summary says so.

- **`amd-case-1`** — artifacts and linkage. **No GPU.** Per artifact: compiles clean (decided on empty output),
  exports only the intercepted HIP names as `GLOBAL DEFAULT` with a non-`UND` section, `NEEDED` is
  `libc.so.6` alone, no `GLIBC_` above `GLIBC_2.4`, zero undefined `hip*`/`hsa*`. Records each artifact's
  `sha256` so the later cases consume what this one produced.
- **`amd-case-2`** — reported capacity, single card. All three entry points report the quota, plus a control
  arm interposing only `hipMemGetInfo` that must still show the physical figure through `totalGlobalMem`.
- **`amd-case-3`** — memory-path completeness. One allocation family per invocation; the pool family is
  charged; the caller-origin baseline (zero self-calls in the allocation family) is asserted.
- **`amd-case-4`** — mask conformance. Every row of the architecture's conformance table as a PASS, every
  fail-open construction for that architecture as a **negative** row, decided by occupancy on a multi-XCC card.
- **`amd-case-5`** — compute semantics. Single-tenant ceiling, disjoint pair, fully-overlapping pair,
  three-tenant fairness, capped-versus-uncapped. Barrier-synchronised, saturating kernel; per-tenant occupancy
  reported alongside throughput wherever `NUM_XCC > 1`.
- **`amd-case-6`** — unit tests, two processes in one container against one quota, and one container across
  two cards with **different** quotas; plus the fill/free/re-request round trip that proves refunds land.
- **`amd-case-7`** — `SIGKILL` reclaim and cross-ROCm-version reach (the same artifact under a 7.x and a 6.x
  container).

#### e2e tests

**None, and the reason is structural rather than an omission.** The operator-side wiring — the detector's
`Status.LogicalSliced`, the allocator's `Sliced` server, and the `pack/` stage that stages the library onto the
host — is a Non-Goal of this spec, so no node can advertise `amd.com/gpu.sliced` when this work lands and no
cluster-level path exercises it. `gpustack-operator-e2e`'s slicing cases are vendor-agnostic and self-skip on
"no `*.sliced`", so they light up for AMD automatically once the handed-off injection spec ships — at which
point they become that spec's acceptance, not this one's. Adding an e2e case here would assert a capability
this spec deliberately does not deliver.

## Alternatives

- **`LD_AUDIT` instead of a preload.** This was the design's first answer and Gate 7 overturned it. The case
  for it rested on three claims and measurement kept only half of one: symbol versioning does **not** obstruct
  a plain preload; `libamdhip64` self-calls exactly one exported symbol (`hipMemGetInfo`) and it is one where
  a self-call is harmless; and a preload does **not** break HIP initialisation — the run that appeared to show
  it did was two of our own defects stacked (an `RTLD_NEXT` miss under a `dlopen`ing framework, plus a
  placeholder return of `1`, which is `hipErrorInvalidValue`). What `LD_AUDIT` genuinely offers is caller
  discrimination through `la_symbind64`'s `refcook`, and today nothing needs it. What it costs is concrete:
  it would be the only such mechanism in a repo where four backends bind-mount `/etc/ld.so.preload`, and audit
  context forbids `pthread_*` and any work in `la_version`, which shapes `common/` around a restriction we do
  not otherwise have. Held as the documented escalation: if the caller-origin diagnostic ever names
  `libamdhip64` under an allocating entry, that is the trigger, and the migration is confined to `hip/`.
- **Enforcing compute in the library with a token bucket or PID loop**, as the Ascend and THead backends do.
  Rejected because the platform already provides what they were built to provide: a hard per-tenant ceiling
  that holds against an idle card, fair sharing under oversubscription, and zero measured overhead. Adding a
  launch-path interceptor would buy nothing measured and cost hot-path latency, a tuning surface, and another
  escape.
- **Refusing to overlap CU masks, i.e. `CoresPercentageOvercommit = false`.** This was the initial reading and
  the measurement overturned it: overlap is permitted by the hardware and shares fairly, so refusing it would
  cap a card at one card's worth of requests for no isolation benefit. Disjoint packing is still preferred
  while capacity allows — it is strictly better when it fits — which is why the policy is "pack disjointly,
  overlap only when oversubscribed" rather than either extreme.
- **Reading topology from KFD sysfs.** Simpler and no CGO, and measured to agree with the HSA agent-info API
  on both architectures — `array_count / simd_arrays_per_engine` equals `NUM_SHADER_ENGINES` on the RDNA card
  (6/2 = 3) and on the CDNA one (32/1 = 32). Rejected anyway, on contract stability rather than on
  correctness: sysfs paths and field semantics are not an interface AMD maintains, the APIs are already bound
  in `binding/hsa` and `binding/amdgpu`, and the derivation needs `NUM_XCC` alongside the rest to pick its
  branch at all.
- **A per-ROCm-major library fan-out**, mirroring NVIDIA's `cuda-12`/`cuda-13`. Measured unnecessary: the
  product links no ROCm object, and one build was observed interposing across two ROCm majors.
- **Deriving the CU quota from `hipDeviceProp_t.multiProcessorCount`.** It is the number a HIP program can
  reach without any binding, which makes it tempting. But it means different things per architecture — WGPs on
  RDNA (30 on a 60-CU card) and CUs on CDNA (304 on a 304-CU card) — so a derivation mixing it with the
  topology APIs is silently out by 2× on one of them and correct on the other, which is the worst way to be
  wrong. It also carries no `NUM_XCC`, which the CDNA branch cannot be written without.

## Open Questions

- ~~**Does a disjoint CU partition isolate a memory-bound workload?**~~ **Answered, negatively, by Gate 8.**
  It does not: a bandwidth-saturating neighbour on the other disjoint half costs a compute tenant a further
  25 % beyond what an equally-sized compute neighbour costs, and half the CUs already reach 97.7 % of the
  card's memory bandwidth. `.sliced` therefore offers a **compute ceiling, not a compute QoS**, and carries no
  bandwidth isolation at all — see Risks, and the wording is now a documentation requirement rather than an
  open question. What is still unmeasured is the *severity envelope*: only one bandwidth pattern (a streaming
  copy) and one victim (a compute-bound kernel) were tried, on one architecture.
- **Do the CDNA rules hold under CPX/DPX compute partitioning, and across multiple cards?** Gate 8 measured
  one `gfx942` agent in SPX/NPS1 under SR-IOV. The host reports `SPX, DPX, QPX, CPX` as available modes, so
  the question is real — a CPX-partitioned card presents several agents each with its own `NUM_XCC`, and the
  derivation is parameterised on the queried value and should follow, but the atom size, the interleave and
  the XCC-coverage requirement have not been confirmed against an agent whose `NUM_XCC` is 1 because the card
  was *split* rather than because the silicon has one XCC. **It was not tested and should not have been on
  that host:** the eight `current_compute_partition` nodes there are owned by `nobody` inside a user
  namespace, seven of the eight cards belong to other tenants, and re-partitioning is a host-wide operation.
  Named experiment, for a machine that is actually ours: re-run Gate 8's occupancy table under CPX, and
  re-run the multi-tenant table across two cards to confirm the per-card `GPU_list` indexing — which one
  card also could not exercise. Needed before an Instinct part is claimed as supported in a partitioned or
  multi-card deployment.
- **Do other RDNA SKUs need a different alignment constant?** The derivation is parameterised on the queried
  shader-engine count, so it should adapt, but only a 3-SE part was measured. `gfx1100` (6 SE) and `gfx1102`
  (2 SE) would confirm that the parameterisation is real rather than a fit.
- **Should the detector hard-fail or soft-warn when the self-check probe fails?** Declining to advertise
  `.sliced` is the safe reading and is what F4 proposes, but it turns a transient probe failure into a
  capability disappearing from the node. The alternative — advertise and log — trades a silent isolation loss
  for availability. This is a product decision, not a technical one.
