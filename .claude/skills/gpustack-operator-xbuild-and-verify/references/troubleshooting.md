# Troubleshooting — xbuild-and-verify

Concrete failure modes hit while building this skill, with fixes.

## Transfer / ssh
- **`scp: Received message too long` / `Ensure the remote shell produces no output`** — a login
  banner (motd, "do not inject conda env", etc.) corrupts the scp/SFTP stream. Don't use scp;
  transfer via base64 over an ssh stdin pipe (what `lib.sh: xput` does):
  `base64 < local | ssh HOST 'base64 -d > remote'`. Strip banner lines from command output
  (`lib.sh: _xb_filter`).
- **`Connection closed by <host> port 22` on the FIRST ssh to a host** — some hardened hosts drop the
  very first connection (host-key prompt / banner / per-connection rate limit). It usually succeeds on
  retry once the key is in `known_hosts`. The trap: `build.sh` probes the arch with `xrun 'uname -m'`,
  and a dropped probe makes it fail with `cannot map target arch 'Connectionclosedby...port22'`. Fix:
  pre-warm the connection once (any `ssh HOST true`) and/or pin `XB_PLATFORM` (e.g.
  `XB_PLATFORM=linux/arm64`) to skip the probe entirely, then re-run.

## Build
- **`BuildKit is enabled but the buildx component is missing or broken`** — Docker 23+ routes
  `docker build` through buildx; the plugin isn't installed. Install the single binary:
  ```bash
  mkdir -p ~/.docker/cli-plugins
  curl -sSL -o ~/.docker/cli-plugins/docker-buildx \
    https://github.com/docker/buildx/releases/download/v0.19.3/buildx-v0.19.3.linux-<arch>
  chmod +x ~/.docker/cli-plugins/docker-buildx   # <arch> = amd64 | arm64
  ```
- **`enpu-monitor` link fails: `undefined reference to drvHdcSessionConnect` /
  `ErrorManager::ATCReportErrMessage`** — the executable can't resolve vendor `.so` cross-refs
  in a toolkit-only image (no host driver). Fixed in `build-libvnpu.sh` by linking with
  `-Wl,--allow-shlib-undefined` (via `LDFLAGS`, which CMake seeds into `CMAKE_EXE_LINKER_FLAGS`).
  An earlier `uname -m`-keyed `libascend_hal.so` `LD_LIBRARY_PATH` probe was wrong: the toolkit's
  `<arch>-linux` tree carries both the host HAL and the device-side (AArch64) HAL, so a path-name
  match picked the wrong-arch stub on amd64. Do not reintroduce it.
- Cross-arch build is slow (qemu); build on a matching-arch host for a fast native build.
- **`ERROR: listing workers: … "error reading server preface: http2: frame too large"`** — the
  docker-container buildx builder is dead, not merely unreachable, and `docker ps` lies about it.
  On a host whose **default runtime is `ascend`**, the builder container is owned by
  `ascend-docker-runtime`, whose vendored runc types `State.init_process_start` as a string while
  containerd writes a number; it therefore cannot report or tear down its own container, so
  dockerd keeps a stale `Up` while `docker inspect` says `exited`, and `restart` fails with
  `could not delete stale containerd task object`. Recover **without losing the build cache** —
  the cache is in the *named* volume `buildx_buildkit_<builder>0_state`, which `docker rm` does
  not delete and which the driver re-attaches by the same deterministic name:
  ```bash
  docker rm -f buildx_buildkit_<builder>0      # succeeds: it is already exited
  docker buildx inspect --bootstrap <builder>  # recreates it on the same state volume
  ```
  This matters when the pinned base image (e.g. `quay.io/ascend/cann:8.5.0-910b-ubuntu22.04-py3.11`)
  is **not** in the host's docker image store: it then lives only in that builder's cache, and
  `docker buildx rm` or switching to the `default` builder forces a ~10 GB re-pull. Corollary:
  under a default `ascend` runtime, prefer short-lived `docker run --rm` containers (as the cases
  do) — any long-lived container can wedge this way.

## Runtime (in-container)
- **`enpu-monitor` exits 139 (SIGSEGV) after "Successfully to initialize vnpu device"** — libdcmi
  not loaded ⇒ weak `dcmi_*` symbols are NULL. Add the libdcmi paths to `ld.so.preload` before
  libvruntime.so (see `ascend-ld-preload-and-libdcmi.md`). Quick check:
  `LD_PRELOAD=/usr/local/dcmi/libdcmi.so enpu-monitor`.
- **`libc_sec.so` / `libboundscheck.so: cannot open shared object file`** — the workload image
  lacks the securec library. Use a CANN-based workload image (the built `vcann-build:*` image is
  exactly CANN-base + artifacts and works as the test image).
- **enpu-monitor prints nothing** — its quota table is written to **stderr**; capture `2>&1`.
- **`npu-smi info` shows full card under injection** — expected; logical-slicing is invisible to
  npu-smi. Use `enpu-monitor` for the slice view (see `ascend-npu-smi-and-aicore.md`).
- **`acl.blas.hgemm` returns `100000` (INVALID_PARAM)** — pyACL low-level BLAS is param-strict and
  is a dead-end for generating AICore load; use a real model (torch_npu/vLLM) instead.

## Container launch (Ascend)
- Use `--runtime=ascend` (or rely on it being the default runtime) with
  `-e ASCEND_VISIBLE_DEVICES=<npu>` so the runtime injects the device + driver libs (npu-smi,
  libdcmi, libascend_hal). Inside the container the visible device is logical `0` regardless of
  the physical index, so `acl.rt.set_device(0)` is correct.

## NVIDIA (HAMi-core / libvgpu.so)
- **`nvidia-smi: command not found` / no GPU in container** — `docker run` must go through the
  NVIDIA container runtime. Either it is the default runtime (`docker info | grep 'Default Runtime'`
  → `nvidia`) with `-e NVIDIA_VISIBLE_DEVICES=<ids>`, or pass `--gpus all` / `--runtime=nvidia`. The
  build image carries no driver; the runtime injects `nvidia-smi` + driver libs.
- **`nvidia-smi` shows the FULL card under injection** — libvgpu.so wasn't preloaded into the
  nvidia-smi process. Check the `/etc/ld.so.preload` mount contains exactly
  `/usr/local/vgpu/libvgpu.so` and that `libvgpu.so` is mounted at that path.
- **Limit didn't change after editing `CUDA_DEVICE_MEMORY_LIMIT`** — HAMi-core caches the shared
  region; a stale `/tmp/vgpu/cudevshr.cache` wins. Start from a fresh cache dir (the nvidia-cases
  `rm -rf` the test dir each run).
- **CUDA probe won't compile on CUDA 13: `cuCtxCreate` arg-count error** — the header macro maps it
  to the `_v4` signature (`CUctxCreateParams`). Use `cuDevicePrimaryCtxRetain(&ctx, dev)` instead.
- **`nvcc … -lcuda`: `cannot find -lcuda`** — link the driver stub:
  `-L/usr/local/cuda/lib64/stubs`.
- **CUDA banner noise in output** — the `nvidia/cuda` image entrypoint prints a license banner; run
  with `--entrypoint nvidia-smi` (or `--entrypoint bash`) to bypass it.
- **SM (compute) limit not visible in `nvidia-smi`** — expected; it is a time-slice throttle, not a
  reported field. Confirm it via the HAMi log `device N: core utilization limit = <N>`
  (`LIBCUDA_LOG_LEVEL=3`) or by sampling `utilization.gpu` under load (see
  `nvidia-smi-and-sm-limit.md`).

## THead (PPU / `libhggc.so` / `libhgml.so`)
- **`ppu-smi` exits 0 even when it fails** — with no driver it prints `init HGML error: driver is not
  loaded` and still returns 0. Its exit status carries no verdict, so every check must parse its
  output. Its `ldd` is also clean of any HGML library, which is the linkage-level confirmation that it
  reaches HGML through `dlopen` at runtime (see `thead-hgml-dlsym-and-ppu-smi.md`).
- **A preload that defines `hgmlDeviceGetMemoryInfo` changes nothing** — `ppu-smi` resolves it with
  `dlsym` on an explicit `dlopen` handle, which never consults the global scope a preload sits in.
  Interpose `dlsym` itself. Check which object won with `dladdr` on the returned pointer rather than
  guessing; `cases/thead-case-2.sh` does this with no hardware.
- **`nm -D <so> | grep dlsym` matches a library that only CALLS `dlsym`** — `nm -D` lists undefined
  symbols too, so it cannot tell "defines" from "references" and will pass an artifact that interposes
  nothing. Use `readelf -W --dyn-syms` and require `GLOBAL DEFAULT` with a non-`UND` section index.
- **`hgml.h` won't compile: `NULL` undeclared / unknown type `bool`** — the header carries zero
  `#include` lines. Supply `<stdbool.h>` and `<stddef.h>` before it, or `gcc -include stdbool.h`.
- **`-Wnonnull-compare` on a `dlsym` interposer** — glibc declares `dlsym`'s `symbol` parameter
  `__nonnull`, so comparing it to `NULL` is dead code the compiler rejects. Drop the check.
- **A shim picks up `DT_NEEDED libdl.so.2` and fails the linkage assertion** — dropping `-ldl` is
  deliberate, not an omission: `dlsym`/`dlvsym` stay undefined in the object and resolve from whatever
  glibc the workload container has. A test *binary* that dlopens does need `-ldl`; a preloaded shim
  must not.
- **`hgMemAlloc` is not the symbol a shim must interpose** — `hggc.h` maps it onto `hgMemAlloc_v2` the
  way `cuda.h` maps `cuMemAlloc`, and `libhggc.so` exports both. The plain form is the v1 ABI with
  different parameter types (`HGdeviceptr_v1` is `unsigned int`), so it cannot be covered by reusing
  the v2 prototype.
- **`hggcMalloc` is a runtime-layer symbol** — it lives in `libhggcrt.13.0.so`, not `libhggc.so`.
  Interposing it would prove nothing about the driver layer; drive it from the test side and interpose
  `hgMemAlloc_v2` instead.
- **An over-quota allocation succeeds and no shim counter moved** — that is the shim watching a name
  nobody called, not the allocation bypassing `libhggc.so`. Read the counter line the shim's
  destructor prints before concluding anything about the interception layer.
- **`ppu-smi` shows a card the container was not given, or renumbers the one it was** — pass only
  `/dev/alixpu`, `/dev/alixpu_ctl` and the one `/dev/alixpu_ppu<N>`, and parse the row whose index
  matches, falling back to the single row when only one is visible.
- **A hardware case picks a busy card** — never hardcode an index. The PPU test host runs production
  inference and one card has held ~91 GB; `lib.sh: thead_idle_cards` reads idle cards out of
  `ppu-smi`'s own table.
- **`nerdctl` on a k3s/rke2 host sees no images** — its containerd namespace defaults away from the
  cluster's. Set `XB_CTR_ARGS='--namespace k8s.io'`. `nerdctl pull` also does not read containerd's
  `config.toml` mirrors, so on a host where `docker.io` times out, pull with `crictl pull` (which goes
  through CRI and therefore the cluster's configured mirrors) and then run with `nerdctl`.
- **No `buildkitd` on the target** — expected, and nothing here needs it: the THead cases only
  `run` containers. Build the image on a docker host and load it. `preflight.sh` reports this as a
  build-capable WARN, not a FAIL.
- **`ERROR: ld.so: object '/tmp/…' from LD_PRELOAD cannot be preloaded … ignored`, and the slice reads the
  physical size** — the preload path is the HOST staging path, but the loader resolving it lives inside the
  container. Name the mount point (`/work/…`), not `${XB_STAGE}/…`. The tell is that the row fails while the
  hardware-free mechanism rows, which hardcode `/work`, pass.
- **`hgmlDeviceGetHandleByIndex` returns `Invalid Argument` for the card you passed through** — the SDK
  **renumbers devices inside the container**: give it one `/dev/alixpu_ppu<N>` and `hgmlDeviceGetCount` reports
  `1` and the only valid index is `0`. The host ordinal names the device node; it is not the index the container's
  SDK addresses. Same for `hgDeviceGet` and `hggcSetDevice`.
- **A card index comes out as a login banner** — `thead_idle_cards` output is consumed directly as an index, so a
  banner line `XB_BANNER_RE` does not cover becomes "the chosen card". The helper now filters to bare digits;
  when adding a similar helper, filter rather than trusting the banner regex to be complete.
- **`refund: freed bytes are returned to the quota` FAILs while all four path groups PASS** — the shim counted the
  frees but did not credit them back, so the whole-quota request after both frees hit our own `DENIED`. Read the
  `accounted=` figure in that marker: non-zero after every allocation was freed is the ledger losing a refund.
  The four path groups cannot catch this — each runs one allocation in a fresh process, so the corrupt total dies
  with it. If you are growing the ledger, note that deleting an entry by **emptying** its slot is what caused it:
  open addressing needs a tombstone, and page-aligned device pointers put nearly every key in one probe chain.
- **`arm c: vendor wrapper coexists` SKIPs with "not loadable in this image"** — expected on an image that ships no
  `libhggc_wrapper.so`; the arm cannot ask about coexistence with something absent. It is deliberately not a PASS:
  the wrapper failing to load leaves the hook working by itself and reporting exactly the quota the arm expects.
  A `could not prove … loaded` FAIL instead means the loader neither initialised it nor said why — check the arm's
  `LD_DEBUG=libs` output for the wrapper's path.
- **`DENIED … device=<i>: no usable HGGC_DEVICE_MEMORY_LIMIT_<i> and no usable HGGC_DEVICE_MEMORY_LIMIT`** —
  fail-closed, working as designed: the shim is only preloaded into sliced containers, so a card being allocated
  on with no figure is a misconfiguration, and letting it through would be an unlimited slice. Both variables are
  named because either could have carried the card — the indexed figure, or the un-indexed one it falls back to.
  Three causes worth separating: neither variable was injected, `<i>` is wrong, or the indexed figure IS set and
  is unusable (which does not fall back — the load-time report names it as `unusable …`). `<i>` is the
  **container-local** index — the SDK renumbers, so a container given `/dev/alixpu_ppu7` addresses it as `0` and
  wants `…_LIMIT_0`, not `…_LIMIT_7`.
- **No `[vppu]` load marker or counter dump, but denials still appear** — expected at the default
  `LIBHGGC_LOG_LEVEL` of `1`, which carries denials and errors only. The markers and the dump are level `2`, which
  is why every case pins it. `0` silences denials as well. Case 1 is unaffected either way: it greps the strings in
  the built object, not runtime output.
- **`not intercepting hgmlDeviceGetMemoryInfo: hgmlDeviceGetIndex unavailable`** — the visibility shim declines to
  rewrite anything rather than apply one card's figure to every card, which on a multi-card container would report
  the wrong number for every card but one. It means the caller's `dlopen` handle could not resolve
  `hgmlDeviceGetIndex`; check that the handle is `libhgml.so` itself and not a wrapper that re-exports only part
  of the surface.
- **`util=others-only`** — `hgmlDeviceGetProcessUtilization` returned samples, but none for the probe's own pid.
  The call passes `lastSeenTimeStamp=0` and so returns all history, which means a neighbouring container's stale
  sample can make the count non-zero. Per-process feedback needs *our* sample, so this is a FAIL rather than the
  `supported` it used to be counted as.
- **`DT_NEEDED` grows `ld-linux-x86-64.so.2` after a change to `common/`** — almost certainly a new `__thread`
  variable. General-dynamic TLS, which is the default model in a shared object, resolves through
  `__tls_get_addr` in the dynamic linker, and that puts `ld-linux` in `DT_NEEDED` — which case 1 fails, because
  the shipped library may need nothing but `libc.so.6`. Mark the variable
  `__attribute__((tls_model("initial-exec")))`: correct here because this library only ever arrives through
  `LD_PRELOAD` or `/etc/ld.so.preload`, so it is always in the initial exec set and never `dlopen`ed.
- **`cannot open the ledger /dev/shm/vppu-ledger`** — the region is where the container's quota is accounted, so
  this is fail-closed: every allocation is then denied with `the ledger is unavailable`. Either `/dev/shm` is not
  writable in this container (the allocator mounts the ledger directory read-write for exactly this reason), or
  `HGGC_LEDGER_PATH` points somewhere that does not exist. It is created lazily by the first process that
  allocates, so a container whose workload never allocated has no file and that is not a fault.
- **`the ledger … is layout version <n> and this build speaks <m>`** — two builds of the library are sharing one
  region, or a stale region file outlived an upgrade. Refusing is deliberate: the layout is a documented contract
  that `tools/` and a future scraper parse by offset, so misparsing it would be worse than declining. Delete the
  file (nothing in it survives a container restart that matters) or point the newer build at its own
  `HGGC_LEDGER_PATH`.
- **`<path> is not a vppu ledger — refusing to overwrite it`** — `HGGC_LEDGER_PATH` names an existing file that is
  not ours. Never overwritten on purpose; point it elsewhere.
- **`device <i> has no free ledger slot`** — more than 32 processes hold a charge against one card, and the
  allocation is refused rather than admitted unaccounted, since an allocation nobody is charged for can never be
  reclaimed either. Dead processes' slots are swept on the path that would otherwise refuse, so this means 32
  *live* processes, which is a workload shape worth looking at before raising the bound.
- **A unit-test row PASSes while its DETAIL column shows an impossible figure** — the assertion called something
  that writes an out-parameter and the row also formats that out-parameter. The order in which a call's arguments
  are evaluated is unspecified, and both orders were seen here: clang evaluated the condition first, the image's
  gcc the varargs first, so the detail was formatted from the value *before* the call. Split the call into its own
  statement. Worth fixing even when the row passes — the same pattern makes a FAIL row print a figure that never
  existed.
- **A forked worker hangs on its first allocation, or gets memory the container's quota was already spent on** —
  both are the same cause: `fork()` duplicates the in-process lock state, so the child inherits either a spinlock
  flag held by a thread that does not exist in it or a thread-local "I hold this card" depth it then treats as its
  own re-entry. `pthread_atfork` is unusable here (`-lpthread` in `DT_NEEDED`), so the state carries its owning pid
  and is reset on first use in a new process. If either symptom appears, check that every entry point which takes
  one of those flags still calls the reset first — case 6's `fork:` row is the guard.
- **A memory path is not refused and no counter moved** — the module never saw it. The counter dump names every
  interposed entry even at zero, so a name absent from the dump is a name the module does not define, and that is a
  different fault from a zero count. Three causes, in the order worth checking: the entry is one of the plain v1
  names and only the `_v2` form is defined (`hgMemAlloc` and `hgMemAlloc_v2` are two symbols, and `hggc.h` maps the
  source name onto the second); the caller resolved the entry through `hgGetProcAddress` on a driver that returns
  its own internal address rather than the interposed one; or the path reached the driver through
  `hgGetExportTable`, whose table is opaque and deliberately not rewritten — the `an opaque table was handed out`
  line at level `2` is what says so.
- **`resolved <name> as <abi-name>: already the interposed entry`** — not a warning. On SDK `2.1.1` the driver's own
  resolver returns the interposed address, so nothing needs substituting; the line exists because a substitution
  that silently matched nothing would be indistinguishable from one that worked. Its absence for an entry the
  module covers is the thing to investigate.
- **`<n> bytes of row padding not accounted, the ledger is full`** — a pitched allocation succeeded and its real
  size (the driver's stride × height) exceeded what was admitted, but the card's process table had no room for the
  correction. The allocation is left alone rather than freed behind the caller's back; the card is under-charged by
  the padding until that process's slot is reclaimed.
- **A wrapper's signature is wrong and nothing catches it** — only possible for the plain v1 names, since every
  other entry is written with the source-level name the header declares, and a suffixed variant takes its type from
  the plain entry with `__typeof__`. The v1 forms need `#undef` to reach at all, which is what removes the header's
  check, so case 1 runs a syntax-only compile with `__HGGC_API_VERSION_INTERNAL` and `__HGGC_API_VERSION_UMD`
  defined — both are needed, and with only the first the header's v1 declarations stay invisible and the row passes
  vacuously. If that row is ever changed, re-prove it by retyping one size: it must fail with
  `conflicting types for 'hgMemAlloc'`.
- **A workload is throttled to a crawl, or not throttled at all, and the `compute` line explains which** — the
  loop prints one line per step at level `2`: `compute device=<i> target=<cap> measured=<util> allow_us=<open>
  period_us=<window> step_us=<interval> graph_util=<avg>/<n> plain_util=<avg>/<n>`. `measured` carrying
  `(unread)` means utilisation could not be read at all, and the loop is then holding the quota's own share of
  the window rather than controlling — check that `libhgml.so` is resolvable inside the container, because the
  library reaches it with `dlopen` (it may not appear in `DT_NEEDED`) and a container without it gets feed-forward
  only. `allow_us` pinned at one hundredth of `period_us` is the floor: the container is asking for more of the
  card than its cap however little it is given, which one long kernel per window is enough to do.
- **The loop oscillates between the whole window and the floor** — it is stepping faster than its sensor answers.
  The driver's per-process utilisation figure is slew-rate limited to about ten percentage points per hundred
  milliseconds in both directions, so a card that went from idle to pinned reads `0, 10, 22, 32 …` and needs a
  full second to say `100`; a loop stepping every 100 ms window therefore acts on a figure up to a second stale in
  the direction it has just moved. `HGGC_SM_CONTROL_STEP_MS` (default 1000) is that interval and is deliberately
  not the window — lowering it to the window reproduces the oscillation, which is how it was found.
- **Every allocation is refused in a container that was sliced for memory** — check the compute figure:
  `HGGC_DEVICE_SM_LIMIT_<i>` for the card, or the un-indexed `HGGC_DEVICE_SM_LIMIT` it falls back to. The
  compute figure became part of the usable-configuration latch when the controller landed, so a container carrying
  a memory figure and no compute figure for **any** card it holds is refused outright, memory allocations included.
  A card's own figure that is set and malformed does not fall back either — the load-time line names the variable
  it rejected (`unusable HGGC_DEVICE_SM_LIMIT_1=abc`), which is the fastest way to tell the two apart. It is deliberate: the
  allocator's own helper defaults a missing compute request to 100%, so treating the variable as optional would
  hand out a whole card's compute silently. `100` is the value to inject when compute is genuinely uncapped.
- **A capped container settles at a fraction of its cap rather than near it** — the utilisation being fed to the
  loop is not the container's own. The sum is taken over this container's processes only, identified through the
  ledger region's process table (the region is per container, so the pids in it are this container's) plus the
  caller's own pid; a filter that let a neighbour's samples in makes each loop read the card total, and two
  containers capped at 25% then settle near 13% each. Case 7's two-container row is the guard, and its floor is
  set to catch exactly that — a floor of "anything non-zero" passes it.

## AMD (ROCm / `libvrocm.so`)
- **The preload is in place and nothing is virtualised** — the container reports the whole card and no
  `[vrocm]` line ever appears. Check, in this order: `/etc/ld.so.preload` is the one **inside the
  container** (the host's is the wrong file and there is no diagnostic for that);
  `LIBVROCM_LOG_LEVEL=2`, because the load marker and the counter dump sit above the default of 1,
  which carries denials only; and `VROCM_LEDGER_PATH`, which has **no default** — without it the
  constructor reports `VROCM_LEDGER_PATH is unset; nothing can be accounted` once and then refuses
  everything, which looks nothing like "quietly did nothing" but is easy to miss on a busy stream.
  A dynamic-loader failure is not silent either, but its one line is easy to lose: glibc prints
  `ERROR: ld.so: object '…' from /etc/ld.so.preload cannot be preloaded … ignored` and then starts
  the process normally. On musl/Alpine there is no line at all.
- **`totalGlobalMem` still shows the whole card while `hipMemGetInfo` shows the quota** — the
  property wrapper is not firing. `hipGetDeviceProperties` is **three** exported symbols at three
  addresses, and ROCm 6+ headers macro-map every source call to `…R0600`; a wrapper on the plain
  name interposes a symbol nothing calls. `hip_props_probe` prints the object each name binds to,
  which is what turns this from a guess into a reading. AMD-CASE 2's second control arm reproduces
  the failure deliberately.
- **A framework allocates past its quota and no counter moved** — the allocation took a door that is
  not wrapped, and the way to find it is a set difference, not a guess: regenerate
  `references/amd-hip-symbol-manifest.md`, whose last two sections list every allocating name the
  runtime exports and then the ones not interposed. Five entry points were found exactly this way
  after the table was believed complete — `hipMalloc3D`, `hipMemCreate` (the virtual-memory family
  PyTorch's expandable-segments allocator uses), and the driver-API halves `hipMemAllocPitch`,
  `hipArrayCreate` and `hipArray3DCreate`. **`hipGraphAddMemAllocNode` is still open** and is
  recorded as a known boundary: a graph node allocates at launch rather than at capture.
- **Nothing is intercepted under a framework that `dlopen`s the runtime** — `RTLD_NEXT` finds
  nothing when `libamdhip64` was not in the initial link map, which is exactly what PyTorch does.
  The resolver falls back to `dlopen(soname, RTLD_NOLOAD|RTLD_LAZY)` over `libamdhip64.so.7`,
  `.so.6` and the plain name, and logs `resolving through <soname>` at level 2 when it takes that
  path. If that line never appears the fallback was not needed — a workload that LINKS the runtime
  never takes it, so its absence is not a fault. Asking for the plain `libamdhip64.so` alone would
  miss, because only a devel image carries that symlink.
- **`build.sh check` fails with `requires GLIBC_2.34`** — glibc moved `libdl` into `libc` at 2.34, so
  `dlopen`, `dlsym` and `dladdr` bind at that version on any modern build host and become the only
  symbols above the floor. Three `.symver` pins in `hip/hip_resolve.c` hold it. `dladdr` is the one
  to forget: it arrives with the caller-origin diagnostic rather than with the resolver, so the floor
  was clean until that diagnostic was added. Any new `libdl` call needs its own pin.
  Note the same figure on an **executable** is not the same problem and cannot be pinned away:
  `__libc_start_main@GLIBC_2.34` and `fstat@GLIBC_2.33` come from the startup stub of anything built
  on a glibc-2.35 image, which is why AMD-CASE 1 asserts the floor for `libvrocm.so` and only
  records it for `rocm-monitor`.
- **A timed compute run reports more than the card can physically do** — the tenants were not
  started together. Without a cross-process barrier each process measures a window in which the
  others had not yet reached their kernel, and N tenants then sum to well over 100 % of the card's
  peak. `cumask_soak`'s file barrier is the fix and every timed row in AMD-CASE 5 uses it. A
  latency-bound kernel is the other half of the same trap: it under-fills a small partition and
  inflates every overlap reading, which is why the kernel is ILP-saturating.
- **A CU mask "works" and the container still gets most of the card** — a rejected mask is silent by
  construction: no error, no log line, no changed return code. On a multi-XCC part this is worse
  than it sounds, because throughput alone cannot see it: `HSA_CU_MASK=0:0` measured a plausible
  3.7 % of the card while **occupying 267 of 304 CUs**, since the seven XCCs the mask never reached
  ran unmasked. Occupancy, read from `HW_ID`, is the only verdict on such a part. `rocm-cumask-check`
  reports both, AMD-CASE 4 asserts the occupancy figure for every fail-open construction, and
  `references/amd-cumask-conformance.md` is where the constructions are listed.
- **`rocm-monitor` says `no usage region`** — nothing in that container has allocated yet. The region
  is created lazily by the first allocation, and the reader opens `O_RDONLY` on purpose: opening with
  `O_CREAT` would leave an empty region behind for every container somebody merely looked at, and the
  next reader could not tell that from a slice that had allocated nothing. Exit 1 is "absent or
  unreadable", exit 2 is "present and unparseable"; the two are deliberately distinct.
