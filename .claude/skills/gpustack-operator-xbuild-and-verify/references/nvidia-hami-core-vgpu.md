# HAMi-core (`libvgpu.so`) — the NVIDIA logical-slicing runtime

The NVIDIA counterpart to Ascend's vcann-rt. Built by the `xbuild-nvidia-cuda-<major>` Dockerfile stage
(product `/out/libvgpu.so`); enforces per-container VRAM + compute (SM) limits by hijacking the CUDA
driver / NVML API.

## What it is
HAMi-core (`github.com/Project-HAMi/HAMi-core`, pinned by the Dockerfile `ARG LIB_HAMI_CORE_COMMIT`)
intercepts calls between the CUDA **runtime** (`libcudart.so`) and the CUDA **driver** (`libcuda.so`),
plus the NVML library (`libnvidia-ml.so`). It is preloaded via `/etc/ld.so.preload` so it loads into every
process in the container — including `nvidia-smi`, whose NVML calls it rewrites.

Unlike Ascend's `enpu-monitor`+`libvruntime.so` pair, HAMi-core ships a **single** `libvgpu.so`. Its
`NEEDED` are hard (not weak): `libcuda.so.1`, `libnvidia-ml.so.1`, `libc.so.6`. Those two NVIDIA libs are
injected by the **NVIDIA container runtime** at `docker run` time (not present in the build image), so the
build stage links fine and the lib only resolves them inside a GPU container. There is **no** libdcmi-style
weak-symbol preload requirement (contrast `ascend-ld-preload-and-libdcmi.md`).

## The injection contract (what the allocator emits)
`pkg/devicemanager/allocator/nvidia/deviceplugin.go: getSlicedContainerAllocateResponse` renders, for a
sliced container:

**Env**
| Var | Value | Meaning |
|---|---|---|
| `NVIDIA_VISIBLE_DEVICES` | csv of GPU ids/UUIDs | which physical cards the runtime exposes |
| `CUDA_DEVICE_SM_LIMIT` | `floor(ratio*100)` | compute (SM) utilization cap, % — fallback for all cards |
| `CUDA_DEVICE_MEMORY_LIMIT_<i>` | `<MiB>m` | per-card VRAM cap (`memory * ratio`), one per allocated card |
| `CUDA_DEVICE_MEMORY_SHARED_CACHE` | `/tmp/vgpu/cudevshr.cache` | cross-process shared-region cache file |

HAMi-core resolution (`multiprocess_memory_limit.c: do_init_device_{memory,sm}_limits`): per-card
`_<i>` wins; else the un-indexed `CUDA_DEVICE_MEMORY_LIMIT` / `CUDA_DEVICE_SM_LIMIT`; SM default 100 (no cap).

**Mounts**
| Container path | Host source | Mode |
|---|---|---|
| `/usr/local/vgpu/libvgpu.so` | `${libDir}/nvidia/cuda-<major>/libvgpu.so` | ro |
| `/etc/ld.so.preload` | `${libDir}/nvidia/ld.so.preload` | ro |
| `/tmp/vgpulock` | host `/tmp/vgpulock` (shared lock) | rw |
| `/tmp/vgpu` | per-pod cache dir | rw |
| `/dev/shm` | `/dev/shm` | rw |

`ld.so.preload` is a single line: `/usr/local/vgpu/libvgpu.so`
(`pack/gpustack-operator/rootfs/etc/gpustack/lib/nvidia/ld.so.preload`).

## Single libvgpu per container → one CUDA major
`libvgpu.so` mounts at one fixed container path, so all cards in a sliced container must share one CUDA
runtime major. The allocator rejects a sliced container spanning CUDA majors (`nvidiaCUDADir` mismatch).
That is why the build is per-major: `xbuild-nvidia-cuda-12`, `xbuild-nvidia-cuda-13`.

## Knobs (HAMi-core)
- `LIBCUDA_LOG_LEVEL` — 0 errors … 3 infos … 4 debug. Level ≥3 prints `Initializing.....` and
  `device N: core utilization limit = <SM>` on CUDA-context init.
- `CUDA_DEVICE_MEMORY_LIMIT` / `_<i>` — VRAM cap (e.g. `4096m`, `1g`).
- `CUDA_DEVICE_SM_LIMIT` / `_<i>` — SM utilization cap, %.
- Stale-cache caveat: changing limits requires removing the shared cache (`/tmp/vgpu/cudevshr.cache`);
  the skill's cases always start from a fresh cache dir.
