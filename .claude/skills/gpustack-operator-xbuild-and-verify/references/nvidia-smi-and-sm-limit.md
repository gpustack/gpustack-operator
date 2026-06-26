# `nvidia-smi` under HAMi-core — what is observable, what is not

The NVIDIA analogue of `ascend-npu-smi-and-aicore.md`. Records what a sliced container's `nvidia-smi` shows once
HAMi-core (`libvgpu.so`) is preloaded, and how the **memory** vs **compute** limits differ in visibility.

## Memory limit — directly visible (NVML hook)
HAMi-core hooks `nvmlDeviceGetMemoryInfo`. Because `libvgpu.so` is preloaded into the `nvidia-smi` process
itself (via `/etc/ld.so.preload`), `nvidia-smi` reports the **virtual** total/used, not the physical card.

Real-hardware result (RTX 4090, physical 49140 MiB):
```
# single card, CUDA_DEVICE_MEMORY_LIMIT_0=4096m
$ nvidia-smi --query-gpu=index,memory.total --format=csv
index, memory.total [MiB]
0, 4096 MiB
# multi card, _0=4096m _1=8192m  (per-card independent)
0, 4096 MiB
1, 8192 MiB
```
This is **not cosmetic**: a CUDA `cuMemGetInfo` from inside the same container returns the same capped total
(`total=4096MiB free=3710MiB`), and an allocation past the cap is denied. nvidia-smi and the CUDA API agree.

## Compute (SM) limit — NOT shown statically; it is a time-slice throttle
`CUDA_DEVICE_SM_LIMIT` caps the **utilization** HAMi-core lets the container's kernels reach, enforced by a
time-slice throttle at kernel launch (`multiprocess_utilization_watcher.c`). `nvidia-smi` has no "max SM%"
field, so a static `nvidia-smi` snapshot does **not** display the SM cap. Two ways to confirm it is applied:

1. **HAMi-core init log** (`LIBCUDA_LOG_LEVEL=3`), printed on CUDA-context creation:
   ```
   [HAMI-core Msg ...]: Initializing.....
   [HAMI-core Info ... multiprocess_utilization_watcher.c:273]: device 0: core utilization limit = 50
   ```
   nvidia-case-2 triggers this with a tiny CUDA probe (`cuDevicePrimaryCtxRetain`).
2. **Empirically, under load** — run a sustained kernel and sample `nvidia-smi --query-gpu=utilization.gpu`;
   it tops out near the cap. This needs a real GPU workload (CUDA app / torch) and is **not** covered by the
   cases yet — same "throttle not auto-verified" gap noted for Ascend AICore in `ascend-npu-smi-and-aicore.md`.

## Why `nvidia-smi` works inside the container at all
The host runs the **NVIDIA container runtime** as the default docker runtime (`Default Runtime: nvidia`), so
`docker run` injects the driver libraries + the `nvidia-smi` binary and honors `NVIDIA_VISIBLE_DEVICES`. The
build image (`nvidia/cuda:*-devel-*`) carries no driver; the runtime provides it. If a host instead needs an
explicit flag, use `--gpus all` (or `--runtime=nvidia -e NVIDIA_VISIBLE_DEVICES=…`).

## CUDA-context probe gotchas (CUDA 13)
- `cuCtxCreate` is the `_v4` signature in the CUDA 13 headers (`CUctxCreateParams`); a 3-arg call won't
  compile. The cases use `cuDevicePrimaryCtxRetain(&ctx, dev)` (stable) to create a context.
- `nvcc … -lcuda` needs the driver **stub**: `-L/usr/local/cuda/lib64/stubs`.
- The `nvidia/cuda` image entrypoint prints a CUDA banner; run with `--entrypoint nvidia-smi`
  (or `--entrypoint bash`) to keep output clean.
