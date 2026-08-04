# The PPU SDK inside `thead-ppu-devel:2.1.1` — layout, headers, glibc floor

What a case can rely on inside the devel image, and the three constraints that have each broken a build.
Everything here was read out of `gpustack/thead-ppu-devel:2.1.1`
(`sha256:5f83fd14370d0dc12929a815962f34bc7ca630b382eb8c330621f25add32da6d`).

## Deployment model: the SDK is in the workload, not on the host

THead follows the **ROCm** model. The PPU SDK is installed in the *workload* container; the host only
passes device nodes through — `/dev/alixpu` (no underscore), `/dev/alixpu_ctl`, and
`/dev/alixpu_ppu<N>` per allocated card. There is no NVIDIA-style container runtime injecting the
driver and management libraries, and no `runtimeClassName` to set: the whole user-space stack resolves
with zero missing libraries in a driverless container.

Two consequences run through every case here:

- A preloaded shim loads inside a container **we do not control**, so its glibc floor is a hard
  constraint rather than a convenience, and it must never hard-link an SDK library.
- A detector cannot gate on `VERSION.txt`: the device-manager holds no SDK, and at detect time the
  workload does not exist yet. Runtime gating has to come from hardware and driver facts.

## Layout

`PPU_HOME` and `PPU_SDK` are both `/usr/local/PPU_SDK`. Top level:

```
LICENSE  NOTICE  VERSION.txt  asight  bin  cfgs  envsetup.sh  include  lib  ppu-smi  release.yaml  targets
```

`VERSION.txt` is two lines, and the build-time gate in `pack/thead-ppu-devel/Dockerfile` asserts the
second one:

```
ppu_sdk_detection_magic
hggcrt_version:v3
```

**Three separate `bin` directories**, which is what made `ppu-smi` easy to hide:

| directory | contents |
| --- | --- |
| `${PPU_HOME}/bin` | compiler toolchain only — `hgcc`, `hglink`, `llvm-*`, `ppu-llc`, `hggc-memcheck` |
| `${PPU_HOME}/ppu-smi/bin` | `ppu-smi` |
| `${PPU_HOME}/asight/bin` | the profiler suite, deliberately left off `PATH` |

The image originally put only the first on `PATH`, so it shipped `ppu-smi` and hid it;
[#75](https://github.com/gpustack/gpustack-operator/pull/75) adds `${PPU_HOME}/ppu-smi/bin` plus a
`command -v ppu-smi` build assertion. **A published tag only carries that after the pack workflow is
re-dispatched**, so anything checking visibility should still invoke `ppu-smi` by absolute path.

Headers and libraries live under `targets/x86_64-linux/`, and `${PPU_HOME}/include` is an identical
copy of `targets/x86_64-linux/include`. The image sets `C_INCLUDE_PATH`, `CPLUS_INCLUDE_PATH` and
`LD_LIBRARY_PATH` to the `targets/x86_64-linux` pair, which is why a bare `dlopen("libhgml.so")`
resolves and why `gcc` finds `hgml.h` even without `-I`. Cases pass `-I` explicitly anyway, so a
change to the image's ENV cannot silently change what they compile against.

The image is **`linux/amd64` only** — the SDK ships `targets/x86_64-linux` and nothing else — so cases
pin `--platform linux/amd64` rather than inheriting an arm64 caller's default.

## `hgml.h` includes nothing at all

`grep -c '#include' hgml.h` is **0**. It supplies neither the `NULL` a caller needs nor the `bool` its
own declaration uses:

```c
hgmlReturn_t hgmlDeviceDestroyVgpuInstance(hgmlVgpuInstance_t vgpuInstance, bool force);
```

Plain C compilation therefore fails unless the consumer supplies both first. Every file under
`csrc/thead/ppu-slicing-shim/` includes `<stdbool.h>` and `<stddef.h>` ahead of the SDK headers;
`gcc -include stdbool.h` is the equivalent for sources that cannot be edited. HAMi-core is a C project,
so a port hits this on its first build.

`hggc.h` is different — it includes `<stdlib.h>` and `<stdint.h>` — but the shims include the same two
headers ahead of it anyway, so the rule is uniform rather than per-header.

Note that the repository's vendored `binding/hgml/hgml.h` is **not** this header: it is a different
generation (541 KB against the SDK's 147 KB) and it even differs in kind — `hgmlDevice_t` is a
struct wrapper there and a plain pointer here. Write against the SDK copy and let the in-image compile
be the check.

## The glibc floor is the real compatibility guard

Not the base image tag. `ARG BASE_IMAGE` defaults to `ubuntu:20.04` (glibc 2.31), matching the SDK
package's own `ubuntu2004` target, and is lowerable to `ubuntu:18.04` (glibc 2.27, verified). What
binds is the build-time assertion: the highest `GLIBC_` version a product requires must not exceed the
SDK's own floor, **2.17**.

```bash
max_glibc="$(readelf -W --dyn-syms "${so}" | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)+' | sort -uV | tail -1 || true)"
highest="$(printf '%s\nGLIBC_2.17\n' "${max_glibc:-GLIBC_2.2.5}" | sort -uV | tail -1)"
[ "${highest}" = "GLIBC_2.17" ]
```

Match every version component: a two-component pattern truncates `GLIBC_2.2.5` to `GLIBC_2.2`, which
can only make the ceiling too lenient. The `|| true` matters because a library requiring no versioned
symbol is legitimate and `grep` exits 1 on no match. The shims currently sit at `GLIBC_2.4`.

`DT_NEEDED` must never name an HGGC or HGML library — the product is preloaded into a container that
brings its own SDK. `libc.so.6` or nothing at all are the only correct answers; a stub small enough to
reference no libc symbol records nothing, because `--as-needed` is Ubuntu's default.

## Version axes do not map onto each other

Package `2.1.1`, SDK directory `ppu-sdk-1.0`, `HGGCRT_VERSION 13000`, API generation `v3`, runtime
library `libhggcrt.13.0.so`, driver version string `2.1.0-ra1f23`. `HGGC_SDK_VERSION` is the
placeholder `"0.0.0-000000"` and must not be used for detection.

## Related

- `thead-hgml-dlsym-and-ppu-smi.md` — why visibility needs a `dlsym` hook.
- `thead-hggc-symbol-manifest.md` — the measured symbol surface and its regeneration command.
- `troubleshooting.md` — the THead section.
