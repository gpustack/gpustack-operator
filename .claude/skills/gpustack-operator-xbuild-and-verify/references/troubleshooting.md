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
