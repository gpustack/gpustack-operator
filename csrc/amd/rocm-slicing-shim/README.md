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
compute is not enforcement but **detection**.

### Computing a mask, injecting it, and checking it are three different jobs

Worth naming, because they are easy to read as one and only the last lives here:

| | job | who | when |
| --- | --- | --- | --- |
| **Compute** | topology + a requested percentage → a mask string, by closed-form arithmetic | the operator's Go allocator | per allocation |
| **Inject** | emit `HSA_CU_MASK` into the container, with `ROCR_VISIBLE_DEVICES` and the memory-limit variables | the operator's device-plugin `Allocate` | per allocation |
| **Enforce** | apply the mask to the workload's queues | **ROCr** — not this library | per process |
| **Check** | run a kernel, read the hardware back, decide whether the mask took effect | `tools/rocm-cumask-check` | once, at detection |

**The mask is derived, never discovered** — integer arithmetic over figures read once from the HSA
agent-info API, with no probing, no trial launch and no fallback. 60 CU / 3 SE / 1 XCC at 50 % is
`0:0-29` and nothing else.

**The check is a separate question, because deriving correctly and being obeyed are not the same
thing and the platform reports neither.** A CU mask fails **open**, in several ways, with no error
and no log line, and the failure costs all of the isolation rather than some of it. On a multi-XCC
card a mask that does not place a bit in every XCC leaves the XCCs it missed running unmasked —
measured, a one-bit mask reads as a healthy 3.7 % slice of throughput while the container can reach
267 of the card's 304 CUs. A probe that judged by throughput would pass it. So `rocm-cumask-check`
runs a kernel and reads `HW_ID` — and `XCC_ID` on multi-XCC parts — back out of the hardware, which
is what separates *honoured*, *discarded* and *honoured on some XCCs only*.

The probe also carries the derivation, under `--percent`, so it has a known-good mask to test
itself with. That is a **reference implementation, not the production path**: the tables in
`references/amd-cumask-conformance.md` are the single source of truth, this copy is the one measured
against real silicon, and the Go one is unit-tested against the same tables.

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
| `libvrocm.so` | `libamdhip64.so` — the HIP runtime | Enforces the VRAM quota and reports it. The classic allocating family, its **driver-API halves** (`hipMemAllocPitch`, `hipArrayCreate`, `hipArray3DCreate` — separate symbols, not aliases), the stream-ordered/pool family, the **virtual-memory-management** family (`hipMemCreate`/`hipMemRelease`), and all three entry points that reach the card's total memory. The list is maintained by subtraction, not by review: see `references/amd-hip-symbol-manifest.md` in the verify skill, whose last sections are every allocating name the runtime exports minus the ones interposed. |
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
device/                  the code that runs ON the GPU — one header, shared by the two
  vrocm_hwid.h           artifacts that read occupancy, so they cannot disagree about it.
                         libvrocm.so includes nothing from here and must not: the product
                         carries no kernel
tools/                   the readers and the probe — preloaded into nothing
testing/                 gate-only artifacts, never shipped in the library
  hip_mem_paths.c        one allocation family per invocation
  hip_props_probe.c      every reported-capacity entry, and which symbol each one bound
  cumask_soak.c          the barrier, the saturating kernel, and per-tenant occupancy
  ledger_lifecycle.c     hold a charge, take SIGKILL, prove the next process gets it back
```

`common/` is where every rule that can be tested without hardware lives, and the no-`hip*`/`hsa*`
rule is what keeps it that way. It also calls no `pthread_*` and no `sem_*`: those carry
`GLIBC_2.34`, and this library is preloaded into workload images whose glibc may be far older, so
the product's ceiling is `GLIBC_2.4`. In-process exclusion is a compiler-atomic spinlock and
cross-process exclusion is an `fcntl()` record lock, both of which predate it.

> **Landed so far:** the whole tree — `build.sh`, `common/`, `hip/`, `device/`, `tools/` and
> `testing/`. Every verb works. What is still outstanding is outside this directory: the
> verification skill's `xbuild-amd-rocm` arm and the `amd-case-*.sh` scripts that drive these
> artifacts.

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
compiler, `OFFLOAD_ARCH` the GPU architectures the artifacts with device code are built for, and
`V=1` traces every command.

`lib`, `test` and the mask probe need HIP or HSA headers, so **the caller decides where they
run** — the verification skill's `xbuild-amd-rocm` arm runs this inside a ROCm devel image, and on
a host with ROCm installed it runs directly. `unit` and `rocm-monitor` need neither, by design.
Nothing here needs a card at build time, including the probe: its architectures are named rather
than detected, because detection would target the build host's card and there is nothing to detect
on a build host with none.

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

## Using the tools

Both are commands someone types, so they are installed with hyphens where their sources are named
with underscores. Neither is preloaded into anything, and neither links the library.

### `rocm-monitor` — what the slice was given, and what it is spending

```bash
rocm-monitor                       # reads the region named by VROCM_LEDGER_PATH
rocm-monitor /var/run/gpustack/vrocm/pod-abc123   # or one named on the command line
```

```
region path=/var/run/gpustack/vrocm/pod-abc123 version=1 cards=64 procs=32
card=0 mem_quota_mib=4096 mem_used_mib=2304 mem_free_mib=1792 lock_holder_pid=0
  proc pid=41 mem_mib=2048 mem_bytes=2147483648
  proc pid=57 mem_mib=256 mem_bytes=268435456
```

It needs **neither ROCm nor a device**, which is what lets it run in the container it is reporting
on, in a sidecar, or on the host against another container's region. The argument comes first so an
operator can point it anywhere; `VROCM_LEDGER_PATH` is the container's own — the allocator gives
each container its own region, because the index a card is charged under is that container's own
position in `ROCR_VISIBLE_DEVICES`. There is no default path: one under `/tmp` would either account
a slice nobody configured, or, on a shared host mount, let unrelated containers collide in one
region.

It **reads and never writes**: it maps the region read-only, without `O_CREAT` and without taking
the card's lock. Creating a region would conjure a slice into existence for anything that merely
looked at the container, and taking the lock would let a monitor wedge behind an allocation that
hung.

Exit codes: **0** the region was parsed · **1** there is no region to read — nothing in this
container has been sliced yet, or the path is wrong, and **nothing was created** · **2** the file
exists and this reader may not parse it (foreign magic, an unknown layout version, slot counts it
was not built for). Refusing is the contract; a reader that guessed at an unknown version would
report figures out of the wrong offsets.

Two things it deliberately does not print. **The compute cap**, because it is not in the region and
could not honestly be put there — compute is enforced by the platform through a CU mask this
library never sees, and whether the hardware honoured it is the next tool's question. And **a card
the container holds but has never allocated on**: a card appears the first time an admission touches
it, so an untouched card is indistinguishable here from one the container does not hold.

### `rocm-cumask-check` — did the compute mask actually take effect?

```bash
rocm-cumask-check                        # derive a 50 % mask for device 0, then verify it
rocm-cumask-check --percent 25 --device 1
HSA_CU_MASK=0:0-14 rocm-cumask-check     # verify a mask already in the environment
```

```
topology device=0 name=gfx1101 cu=60 se=3 sa_per_se=2 xcc=1 unit=wgp units=30
mask source=HSA_CU_MASK value=0:0-29
PASS | mask/parses | syntax is GPU_list:CU_list[;...]
PASS | mask/applies_to_device | a segment names this device
PASS | mask/bits_in_range | highest index 29 against 60 CUs
PASS | mask/wgp_pairs_whole | 15 whole WGP pairs, 0 split
PASS | occupancy/units_match | WGP: masked 15, occupied 15
FAILS=0
```

**It runs the Check row above, and only that row** — it does not decide any workload's mask, and
nothing it prints reaches an allocation. It reads the card's topology through the HSA agent-info
API, gets a mask under test, runs a kernel under it, and has every wave report its own physical
identity out of `HW_ID` — plus `XCC_ID` on multi-XCC parts. PASS means the units its waves ran on
are the units the mask asked for; anything else is the finding.

**Where the mask under test comes from is the only thing the two modes differ in.** With
`HSA_CU_MASK` or `ROC_GLOBAL_CU_MASK` already in the environment it verifies that one as it stands
— which is how a case reproduces each fail-open construction, and how you would check a mask an
allocator actually emitted. With neither set it derives one for `--percent` and **re-execs itself**:
ROCr reads `HSA_CU_MASK` while it initialises, before any code here could set it, so a probe that
stayed in-process would measure the environment it started with.

**It counts, and never times.** It compares the number of distinct units occupied — and on a
multi-XCC part the count in every XCC — against what the mask asked for. It does not compare
throughput, because throughput passes the worst failure there is: see the section above.

Exit codes: **0** the mask took effect as asked · **1** it did not, which is the finding this tool
exists to make · **2** the probe could not run (no agent, a request below one allocation atom, a
malformed argument). Its intended caller is the detector, which should decline to advertise a
sliced capability for a card that fails rather than advertise one the node will not honour.

The rules it applies, the masks it accepts and the constructions it must reject are all measured,
and they are checked in as
`.claude/skills/gpustack-operator-xbuild-and-verify/references/amd-cumask-conformance.md` — the same
fixture the operator's Go-side derivation is tested against.

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
