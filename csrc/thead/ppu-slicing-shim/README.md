# `libvppu` — THead PPU logical-slicing shims

Preloaded libraries that give one container a **slice** of a PPU card rather than the whole card:
a per-card VRAM quota that is enforced, a compute cap that is throttled, and figures that
`ppu-smi` reports as the slice instead of the device. Beside them, one reader that prints what the
slice was given and what it is spending.

They are injected into a **workload** container by the GPUStack device-plugin allocator, through
`/etc/ld.so.preload`; the reader is mounted alongside and run by hand. Nothing here runs on the
host, and nothing here is loaded into the operator itself.

## Why a preload, and why these libraries

THead's SDK is deployed ROCm-style: the **container** brings `libhggc.so` (driver layer),
`libhggcrt*.so` (runtime layer) and `libhgml.so` (management), and the host passes through only
the device nodes `/dev/alixpu`, `/dev/alixpu_ctl` and `/dev/alixpu_ppu<N>`. There is no host
library to replace and no NVIDIA-style injection point, so the only seam is symbol interposition
inside the workload's own process.

| Artifact | Interposes | What it does |
| --- | --- | --- |
| `hggc_quota.so` | `libhggc.so` — the driver layer | Enforces both quotas. 38 memory names (allocate, free, query, host memory, mapping, pools, and the entry-point resolvers) plus 16 launch entries. |
| `hgml_dlsym_hook.so` | `dlsym` itself | Makes the slice **visible**. `ppu-smi` `dlopen`s `libhgml.so` and resolves on that explicit handle, so a defined HGML symbol would never be reached; hooking `dlsym` is what gets in front of it. Both memory getters are wrapped separately — their shared helper is `FUNC LOCAL` — and the `used` they report comes from the same ledger the quota is enforced against, so the two halves show one number. |
| `ppu-monitor` | nothing — it *reads* | Prints the slice: both quotas and both usages per card, including the compute **limit**, which appears in no `ppu-smi` field. Preloaded into nothing; it parses the usage region, which is the path a metrics scraper takes too. |

**Two shared objects rather than one**, one per interposed library. They are injected together and
share `common/`, but they are not the same job: enforcement has to be in every process that
allocates, while the `dlsym` hook is a far more invasive interposition that only matters in a
process asking HGML about the card. Keeping them apart means either can be injected without the
other, and it is why `common/` is compiled into each of them rather than into a library between
them.

The quota library needs no `dlsym` hook: the runtime layer lists `libhggc.so` in `DT_NEEDED` and
reaches it through the PLT, so a preloaded definition wins by plain interposition.

## Layout

```
build.sh                 the only place that knows how to compile this tree
common/                  no hg*/hggc*/hgml* type may appear here — that rule is what makes it
                         testable with no SDK and no device
  vppu.h                 the log channel, the level, the device bound
  vppu_log.c             verbosity
  vppu_quota.{h,c}       the environment contract, the load-time report, the usable/unusable latch
  vppu_ledger.{h,c}      the cross-process usage region, the per-card lock, charges, the key map
  vppu_pid.{h,c}         the compute controller's arithmetic
  vppu_test.c            this library's unit tests
hggc/                    the enforcement half — one shared object over the whole driver layer
  hggc_quota.h           the module's private contract: the entry list and both decisions
  hggc_quota.c           the memory admission decision, the entry table, the call counters
  hggc_mem.c             the current-ABI memory entries
  hggc_mem_v1.c          the v1 ABI names, whose parameter types differ
  hggc_entry.c           hgGetProcAddress{,_v2} and hgGetExportTable
  hggc_compute.c         the duty-cycle window, the PID loop, HGML sampling
  hggc_launch.c          the 16 launch entries
hgml/                    the visibility half — one shared object over the dlsym hook
  hgml_dlsym_hook.c      the hook, the two memory getters, the re-entrancy and origin guards
tools/                   preloaded into nothing: it reads the region the other two write
  ppu_monitor.c          -> ppu-monitor, the per-card quota/usage reader
testing/                 gate-only artifacts, never shipped in the library
  hgml_nohook.c          the control: the same HGML symbols, no dlsym hook
  dlsym_stack.c          a second dlsym interposer, to stack against the hook in both orders
  dlsym_origin.c         which object won a symbol, by dladdr
  hgml_util_probe.c      what hgmlDeviceGetProcessUtilization actually reports
  hggc_mem_paths.c       one memory path per invocation, plus the hold path
  hggc_launch_load.cu    a spin kernel, the only way to occupy a card
```

Two rules the tree lives by, both asserted rather than trusted (see *Verifying*):

- **A shipped object links nothing but libc.** `DT_NEEDED` stays empty or exactly `libc.so.6`,
  and the highest `GLIBC_` symbol version it requires is ≤ 2.17 — the SDK's own floor, because
  this loads into whatever the workload's base image is. Every vendor symbol is resolved at
  runtime through the `dlsym` chain. That is also why the lock in `common/` is an `fcntl` record
  lock rather than a `pthread_mutex` (which would need `-lpthread`), and why a `__thread`
  variable here carries `tls_model("initial-exec")` (the default model pulls in the dynamic
  linker).
- **Only the interposed vendor names are exported.** Everything internal is `VPPU_INTERNAL`
  (hidden), because a preloaded library that exported its own seam would be interposable by the
  very workload it polices.

## Building

`build.sh` is the single entry point. It is **silent when it succeeds** — `V=1` traces each
command — because the verification cases decide "compiles clean" on empty output.

```bash
./build.sh lib          # hggc_quota.so, hgml_dlsym_hook.so (+ the gate control hgml_nohook.so)
./build.sh tool         # the readers under tools/ -> ./ppu-monitor
./build.sh test         # the gate binaries under testing/
./build.sh unit         # common/'s unit tests -> ./vppu_test
./build.sh check v1     # have hggc.h itself type-check the v1 prototypes
./build.sh list hggc_quota    # the translation units behind one artifact
```

`lib` and `test` need the **PPU SDK headers**, which ship only inside the vendor image, so they
run either on a host with the SDK installed or inside `gpustack/thead-ppu-devel:<version>`:

```bash
docker run --rm -v "$PWD:/work" -w /work gpustack/thead-ppu-devel:2.1.1 \
  bash -lc './build.sh lib && ./build.sh test'
```

`unit` and `tool` need neither the SDK nor a device — that is the point of the `common/` rule, and for
the reader it is a product requirement rather than a convenience: it is mounted into containers that
may hold no card at all. Both run anywhere, including a macOS development machine:

```bash
./build.sh unit && ./vppu_test          # prints STATUS | CHECK | DETAIL rows, ends in FAILS=<n>
```

Env: `OUT` (where artifacts land, default this directory) · `PPU_HOME` (SDK root, default
`/usr/local/PPU_SDK`) · `CC` · `HGCC` (the vendor's device compiler, for the one `.cu` under
`testing/`) · `V=1`.

## Using

In production the allocator injects everything below. To reproduce a slice by hand, preload the
library and give the container its figures:

```bash
docker run --rm \
  --device /dev/alixpu --device /dev/alixpu_ctl --device /dev/alixpu_ppu3 \
  -v "$PWD:/work" \
  -e LD_PRELOAD=/work/hggc_quota.so \
  -e HGGC_DEVICE_MEMORY_LIMIT_0=4096 \
  -e HGGC_DEVICE_SM_LIMIT=25 \
  -e LIBHGGC_LOG_LEVEL=2 \
  gpustack/thead-ppu-devel:2.1.1 ./your-workload
```

`<i>` is the **container-local** device index, not the host ordinal: the SDK renumbers devices
inside a container, so one card passed through is index `0` whatever `/dev/alixpu_ppu<N>` it came
from.

A container holding **several** cards gets one figure per card, and each allocation is charged to the card it
actually lands on: the calling thread's context normally, but `prop->location.id` for the VMM path and the pool's
own card for a pool allocation, because those name a card that need not be the context's.

| Variable | Meaning | Absent |
| --- | --- | --- |
| `HGGC_DEVICE_MEMORY_LIMIT_<i>` | per-card VRAM cap, MiB | **init error** |
| `HGGC_DEVICE_SM_LIMIT` | compute cap for the container, percent | **init error** |
| `LIBHGGC_LOG_LEVEL` | `0` silent · `1` denials and errors · `2` also load markers, the per-entry counter dump and one line per control step | defaults to `1` |
| `HGGC_LEDGER_PATH` | the cross-process usage region's file | `/dev/shm/vppu-ledger` |
| `HGGC_SM_CONTROL_PERIOD_MS` | the compute controller's gating window | `100` |
| `HGGC_SM_CONTROL_STEP_MS` | how often the loop steps | `1000` |
| `HGGC_SM_CONTROL_KP` / `_KI` / `_KD` | the loop's gains, in hundredths | `25` / `8` / `0` |
| `HGGC_SM_GRAPH_WEIGHT` | launches' worth of window a graph launch is charged | `1` (off) |

**Both caps are an init error when absent, deliberately.** This library is only ever preloaded
into a container that is being sliced, so a missing figure is a misconfiguration and not "no
limit" — the reference implementation that reads a missing config as "not in a container" hands
out the whole card, which is the one outcome this design refuses to have. An unusable
configuration is reported at load and then refuses every allocation and every launch; it does not
`_exit()`, because arriving through `/etc/ld.so.preload` means exiting would kill every process
in the container, including the shell someone would diagnose it from. Compute that is genuinely
uncapped is written `HGGC_DEVICE_SM_LIMIT=100`.

One more ordering rule, and it is not this library's to enforce: **the visibility shim must be
preloaded before any other library that interposes `dlsym`.** Two such libraries do not chain
through each other — a versioned `dlvsym(RTLD_NEXT, "dlsym", …)` lookup steps over an unversioned
definition — so whoever the loader reaches first owns the symbol and the other one is loaded,
initialised and never entered. Behind a peer, this shim is inert and `ppu-smi` reports the
physical card. `cases/thead-case-2.sh` pins both directions.

### The compute cap, and the five knobs behind it

The cap is enforced by gating launches: per card the shim keeps a repeating window
(`HGGC_SM_CONTROL_PERIOD_MS`), lets launches through while the window is open, and makes later
ones wait for the next one. How far it opens the window is decided by a **PID controller** — a
feedback loop that adds up three terms computed from the error between the cap you asked for and
the utilisation actually measured: the present error (**P**), the accumulated past error (**I**),
and its rate of change (**D**), each scaled by a gain. It is the standard way to hold a measured
quantity at a setpoint without knowing the system's model; the general form is described at
<https://en.wikipedia.org/wiki/PID_controller>.

The loop is dynamic; **the gains are not.** `HGGC_SM_CONTROL_KP` / `_KI` / `_KD` are read from the
environment, so they are fixed for the life of a container and there is no auto-tuning: the
defaults (`25` / `8` / `0`, in hundredths) were chosen against a **simulated** card in `common/`'s
unit tests and are **not fitted to PPU hardware**, so a workload whose shape differs may want its
own. `HGGC_SM_CONTROL_STEP_MS` is the one that is not a preference: it is the driver's *measured*
settling time — its per-process utilisation figure moves about ten percentage points per 100 ms
however abruptly the load changes — and lowering it towards the window makes the loop act on a
figure that has not caught up and oscillate across the whole range.

Tuning one is a measurement, not a guess, and the shim publishes what you need for it. Run the
real workload at the cap and watch either stream:

```bash
# one line per control step, on stderr
[vppu] compute device=0 target=25 measured=24 allow_us=18460 period_us=100000 step_us=1000000 …
# or, without the log noise, the same figures out of the region while it runs — see below
```

| What you see | Knob | Why |
| --- | --- | --- |
| `measured` settles a few points **under** `target` and stays there | raise `_KI` | the accumulated error is what closes a steady offset; `_KP` alone cannot |
| `measured` **swings** across the range, `allow_us` slamming between its floor and the whole window | raise `_STEP_MS` first, then lower `_KP` | acting faster than the sensor settles is the usual cause, not too little gain |
| `measured` takes many seconds to reach `target` after a burst starts | raise `_KP` | the present error is the term that reacts to a change |
| `measured` is noisy but centred | leave `_KD` at 0 | the feedback is a sampled figure, so differentiating it mostly amplifies sampling noise |

Change one knob at a time and re-read the same two figures; `target` versus `measured` in a steady
state is the whole verdict. `cases/thead-case-7.sh` is the same measurement automated, with bands
instead of a human eye.

### Reading what it did

At `LIBHGGC_LOG_LEVEL=2` every line is tagged `[vppu]`:

```
[vppu] hggc_quota loaded, per-card HGGC_DEVICE_MEMORY_LIMIT_<i>, 54 entries
[vppu] DENIED hgMemAlloc_v2 device=0 request=8589934592 accounted=0 quota=4294967296
[vppu] compute device=0 target=25 measured=24 allow_us=18460 period_us=100000 step_us=1000000 …
[vppu] hggc_quota counters: hgMemAlloc_v2=1 … hgLaunchKernel=2049 …
```

- a `DENIED` line is a refusal **by this quota**, which is what distinguishes it from a failure
  for any other reason;
- the counter dump at exit names every interposed entry even at zero, so "the call reached
  `libhggc.so`" is decided by counting rather than inferred from linkage.

### Reading the usage region (`HGGC_LEDGER_PATH`)

`HGGC_LEDGER_PATH` names an ordinary file — `/dev/shm/vppu-ledger` by default — that every process
in the container maps. It is the cross-process ledger *and* the usage surface: a slice's quota, what
it has spent and what the compute loop is doing appear in **no `ppu-smi` field**, so this file is
where they can be read at all. Read it with anything; the layout is a contract, not an internal
detail, and it is what a monitoring scraper is meant to use. Host byte order, fixed offsets, no
lock on the read side — a figure one allocation stale beats a reader that can block behind a
vendor allocation.

```
offset  bytes  field
     0      8  magic "VPPUREGN"          -- absent or different: not a ledger, do not parse
     8      4  layout version (1)        -- unknown version: refuse, do not guess
    12      4  header_bytes (96)         -- where devices[] starts
    16      4  device slots (64)
    20      4  process slots per card (32)
    32     64  lock arena                -- one byte per card, locked by offset, never data
    96    576  card 0, then one 576-byte slot per card: 96 + 576 * <card>
```

Inside a card's slot:

```
 +0      8  memory quota, bytes          -- the figure in force, re-read from the env, not frozen
 +8      8  memory accounted, bytes      -- what this container is charged for on this card
+16      4  compute limit, percent
+20      4  compute utilisation, percent -- what the loop last measured for this container
+24      4  pid holding the card's lock  -- 0 when free; names the process a hung allocation is in
+32     32  the controller: window start ns, allow ns, last step ns, integral, last error (2 × i32)
+64    512  32 × { pid i32, reserved u32, bytes u64 } -- the per-process breakdown
```

So for card `N` at `Q=$((96 + 576 * N))`:

```bash
head -c 8 /dev/shm/vppu-ledger                          # VPPUREGN, or it is not ours
od -A d -t u4 -j 8   -N 16 /dev/shm/vppu-ledger         # version, header_bytes, slot counts
od -A d -t u8 -j 96  -N 16 /dev/shm/vppu-ledger         # card 0: quota, accounted (bytes)
od -A d -t u4 -j 112 -N 12 /dev/shm/vppu-ledger         # card 0: sm limit, sm util, lock holder
od -A d -t u8 -j 128 -N 24 /dev/shm/vppu-ledger         # card 0: window start, allow, last step (ns)
od -A d -t d4 -j 152 -N 8  /dev/shm/vppu-ledger         # card 0: integral, last error (signed)
od -A d -t d4 -j 160 -N 8  /dev/shm/vppu-ledger         # card 0: first charged pid
```

`allow_ns` against the period is the throttle as it stands: `allow / period` is the fraction of
each window the card is currently open for. A card the container never touched is all zeros, and
zero quota with a non-zero `accounted` cannot happen — both are written under the card's lock.

### `ppu-monitor`, which reads it for you

`tools/ppu-monitor` prints the same figures per card, so nobody has to keep the offsets in their head:

```console
$ ppu-monitor
region path=/dev/shm/vppu-ledger version=1 cards=64 procs=32
card=0 mem_quota_mib=4096 mem_used_mib=1024 mem_free_mib=3072 sm_limit_pct=25 sm_util_pct=7 allow_us=18460 lock_pid=0 mem_quota_bytes=4294967296 mem_used_bytes=1073741824
  proc pid=4242 mem_mib=1024 mem_bytes=1073741824
```

It is **not preloaded into anything** — it opens `HGGC_LEDGER_PATH` read-only and parses it, which is
why it needs neither the SDK nor a card, and why the same shape works for a scraper. It links none of
`common/`'s ledger code on purpose: that code maps the region lazily, which means it *creates* one
when none exists, and its other entries take the card's lock — a reader must do neither.

`allow_us` is printed raw rather than as a percentage, because the window it is a fraction of is
**not in the region**: the period is the container's own `HGGC_SM_CONTROL_PERIOD_MS`, and a reader
that cannot see that environment would print a confident wrong number. `sm_limit_pct` is the figure
nothing else reports at all.

| Exit | Meaning |
| --- | --- |
| `0` | the region was parsed; cards printed, or a `#` line saying none has been charged yet |
| `1` | there is no region — nothing in this container has been sliced, or the path is unreadable |
| `2` | the file exists and this reader may not parse it: foreign magic, unknown layout version, or slot counts it was not built for |

A card the container holds but has never allocated on has **no row**: the region records a card the
first time an admission touches it. The full contract, for anything reading this file that is not
`ppu-monitor`, is
`.claude/skills/gpustack-operator-xbuild-and-verify/references/thead-usage-region.md`.

## Verifying

The assertions live with the verification skill, not here:
`.claude/skills/gpustack-operator-xbuild-and-verify` — build the artifacts once, then run the
numbered THead cases. Case 1 needs no PPU; the rest do, and `SKIP` rather than pass without one.

```bash
export XB_MODE=ssh XB_HOST=root@<ppu-host> XB_CTR=nerdctl XB_CTR_ARGS='--namespace k8s.io'
bash .claude/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh xbuild-thead-ppu
bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-1.sh   # build + linkage
… cases/thead-case-{2..7}.sh
```

`common/`'s unit tests are the one part that needs no hardware at all, and running them directly
(`./build.sh unit && ./vppu_test`) is the fastest loop while changing the ledger, the quota
parsing or the controller arithmetic.
