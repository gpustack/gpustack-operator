# `libvrocm` — AMD GPU logical-slicing shim

A preloaded library that gives one container a **slice** of an AMD GPU rather than the whole card:
a per-card VRAM quota that is enforced, and a capacity that every ROCm query reports as the slice
instead of the device. Beside it, one reader that prints what the slice was given and what it is
spending, and one probe that decides whether the card's **compute** mask actually took effect.

It is injected into a **workload** container by the GPUStack device-plugin allocator, through
`/etc/ld.so.preload`; the tools are mounted alongside and run by hand or by the detector. Nothing
here runs on the host, and nothing here is loaded into the operator itself.

## What this enforces, and what it does not

**Memory is enforced here. Compute is not, and cannot be.**

ROCm enforces a compute quota in hardware, through a CU mask the runtime reads out of
`HSA_CU_MASK` before this library is in a position to influence anything. There is no launch
entry point worth throttling and no reason to put one on the hot path. So what this tree ships for
compute is not enforcement but **detection**: `rocm-cumask-check` runs a kernel under a derived
mask and reads `HW_ID` — and `XCC_ID` on multi-XCC parts — back out of the hardware, so the node
can tell a mask that took effect from one the runtime silently discarded.

That distinction is not academic. A CU mask fails **open**, in several ways, with no error and no
log line, and the failure costs all of the isolation rather than some of it. On a multi-XCC card a
mask that does not place a bit in every XCC leaves the XCCs it missed running unmasked — measured,
a one-bit mask that reads as a healthy 3.7 % slice of throughput while the container can reach 267
of the card's 304 CUs. A probe that judged by throughput would pass it.

## Why a preload, and why the HIP layer

ROCm is deployed container-side: the **container** brings `libamdhip64.so` (the HIP runtime),
`libhsa-runtime64.so` (ROCr) and the rest, and the host passes through only `/dev/kfd` and the DRM
render nodes. There is no host library to replace and no NVIDIA-style injection point, so the only
seam is symbol interposition inside the workload's own process.

Interposition is at the **HIP runtime layer**, and the boundary that creates is acknowledged
rather than papered over: `hsa_amd_memory_pool_allocate` and a direct `AMDKFD_IOC_*` `ioctl` are
defined by other objects and never reach these wrappers. That is a property of the design, not a
defect in it, and it is why this is not a security boundary — see below.

| Artifact | Interposes | What it does |
| --- | --- | --- |
| `libvrocm.so` | `libamdhip64.so` — the HIP runtime | Enforces the VRAM quota and reports it. The classic allocating family, the stream-ordered/pool family, and all three entry points that reach the card's total memory. |
| `rocm-monitor` | nothing — it *reads* | Prints the slice: the quota in force and what is charged against it, per card. Preloaded into nothing; it parses the usage region, which is the path a metrics scraper takes too. |
| `rocm-cumask-check` | nothing — it *probes* | Derives a mask for the card it is pointed at, runs under it, and reports whether the CUs it actually ran on are the ones the mask asked for. |

**One shared object rather than several**, because there is one interposed library. And **one
build for every ROCm version**: `libvrocm.so` links no ROCm object at all — every real entry point
is reached through the resolver at run time — so a `7.2`-built artifact interposes correctly
inside a ROCm `6.4` container, and inside a PyTorch wheel that brought its own 6.4 runtime.

## Layout

```
build.sh                 the only place that knows how to compile this tree
common/                  no hip*/hsa* type may appear here — that rule is what makes it
                         testable with no ROCm installed and no device present
  vrocm.h                the shared vocabulary and the device bound
  vrocm_log.{h,c}        the three levels, and the one channel they gate
  vrocm_quota.{h,c}      what the container was given, and whether it is usable
  vrocm_ledger.{h,c}     the cross-process region, its per-card lock, and the admission
  vrocm_test.c           common/'s unit tests — no ROCm, no device, runs anywhere
hip/                     the interposed entry points and the resolver
tools/                   the readers and the probe — preloaded into nothing
testing/                 gate-only artifacts, never shipped in the library
```

`common/` is where every rule that can be tested without hardware lives, and the no-`hip*`/`hsa*`
rule is what keeps it that way. It also calls no `pthread_*` and no `sem_*`: those carry
`GLIBC_2.34`, and this library is preloaded into workload images whose glibc may be far older, so
the product's ceiling is `GLIBC_2.4`. In-process exclusion is a compiler-atomic spinlock and
cross-process exclusion is an `fcntl()` record lock, both of which predate it.

> **Landed so far:** `build.sh` and `common/`. `build.sh` declares every artifact this tree will
> carry, including the ones whose sources are not written yet, so `lib`, `tool` and `test` fail
> until they are; `unit`, `list` and the mutation checks work today.

## Building

`build.sh` is the only place that knows the translation units, the include roots and which
artifact may see a ROCm header. It is **silent when it succeeds** — the verification cases decide
"compiles clean" on empty output rather than on the exit status, so anything it printed of its own
would read as a compiler diagnostic.

```bash
./build.sh lib                # libvrocm.so — the product
./build.sh tool               # rocm-monitor, rocm-cumask-check
./build.sh test               # the gate binaries under testing/
./build.sh unit               # common/'s unit tests — needs neither ROCm nor a device
./build.sh unit mutants       # re-run those tests against deliberately broken builds
./build.sh check              # the four linkage assertions
./build.sh list libvrocm      # the translation units behind one artifact
```

`OUT` chooses where artifacts land, `ROCM_PATH` the SDK root (default `/opt/rocm`), `CC` the
compiler, and `V=1` traces every command.

`lib`, `test` and the mask probe need HIP or HSA headers, so **the caller decides where they
run** — the verification skill's `xbuild-amd-rocm` arm runs this inside a ROCm devel image, and on
a host with ROCm installed it runs directly. `unit` and `rocm-monitor` need neither, by design.

### The four linkage assertions

`build.sh check` is the product's entire external contract, and each line of it has a failure
behind it:

1. **It exports only the HIP entry points it interposes.** A `common/` symbol reaching the global
   namespace would itself be interposable by the workload.
2. **`DT_NEEDED` is exactly `libc.so.6`.** A second entry is a library the workload image may not
   have.
3. **No `GLIBC_` requirement above `GLIBC_2.4`.** This one fails loudly if the resolver's own
   `dlopen`, `dlsym` and `dladdr` are not `.symver`-pinned — glibc moved all three into `libc` at
   `2.34`, and they are the only symbols in the product that ever cross the line.
4. **Zero undefined `hip*`/`hsa*` symbols.** This is what makes one build serve every ROCm
   version; a link-time dependency would tie the artifact to the version it was built against.

### The mutation checks

`build.sh unit mutants` re-runs `common/`'s tests against four builds, each broken in exactly the
way one of the ledger's behavioural properties forbids: the quota frozen at region creation, the
lock released between the check and the charge, a full tracking table dropping records silently,
and the liveness sweep removed. **Each mutant must make its named test row FAIL.** A test that
passes against the broken build is decoration and is rewritten rather than kept.

## Verifying

The tests here are unit tests; the ones that need a card are the `amd-case-*.sh` scripts in the
`gpustack-operator-xbuild-and-verify` skill. Case 1 needs no GPU and re-runs the four linkage
assertions above; the rest need a real AMD host, and the compute cases assert a different
conformance table depending on whether the card reports one XCC or several.

## This is not a security boundary

Say so wherever it is described. Removing the preload restores the whole card's memory; `env -u
HSA_CU_MASK` restores the whole card's compute; `HSA_CU_MASK_SKIP_INIT=1` does the latter without
removing anything; a child process started with a cleared environment loses the quota variables;
and `rocm-smi` reads sysfs and the DRM nodes rather than HIP, so it reports the physical card
under any quota. The mechanism keeps a cooperative workload inside its slice. It does not keep an
uncooperative one there.
