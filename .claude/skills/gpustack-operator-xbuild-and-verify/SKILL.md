---
name: gpustack-operator-xbuild-and-verify
description: "Build and verify the GPUStack Operator's accelerator logical-slicing **builder stages** (`xbuild-ascend-cann-*` and `xbuild-nvidia-cuda-*` in `pack/gpustack-operator/Dockerfile`) end to end, either on the local docker host or on a remote accelerator host over ssh. Builds one stage via buildx `--target`, then runs numbered cases against the produced runtime. SCOPE — two backends: **Ascend (vcann-rt: `libvruntime.so` + `enpu-monitor`)** and **NVIDIA (HAMi-core: `libvgpu.so`)**. Ascend cases: (1) artifacts+linking [no NPU], (2) inject + `enpu-monitor`, (3) memory-quota enforcement. NVIDIA cases: (1) artifacts+linking [no GPU], (2) single-card inject + `nvidia-smi`/SM-limit, (3) multi-card per-device limits. The hardware cases need a real accelerator. Proactively offer this whenever a branch changes the Docker build flow — `pack/gpustack-operator/Dockerfile` or `pack/gpustack-operator/external/(ascend|nvidia)/**`. Examples: \"verify my Dockerfile build-stage change\", \"did the vcann-rt / HAMi-core build still link\", \"test the logical-slicing build on the 910B / 4090 host\", \"does enpu-monitor still work in a container\", \"does nvidia-smi show the sliced memory\", \"prove the memory slice is enforced on real hardware\"."
allowed-tools: "Read, AskUserQuestion, Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/scripts/preflight.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/ascend-case-1.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/ascend-case-2.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/ascend-case-3.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-1.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-2.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-3.sh*), Bash(grep*), Bash(git diff*), Bash(git rev-parse*), Bash(ssh*), Bash(docker buildx*), Bash(docker images*), Bash(docker info*), Bash(command -v*)"
model: sonnet
---

# GPUStack Operator — accelerator xbuild & verify

Build one logical-slicing builder stage from `pack/gpustack-operator/Dockerfile` and verify the runtime it produces, on the local docker host or on a remote accelerator host over ssh. Two backends:

- **Ascend (vcann-rt).** `xbuild-ascend-cann-*` → `libvruntime.so` + `enpu-monitor`. Verifies artifacts/linking, the `npu_info.config` injection, and **real memory-quota enforcement** on a real NPU.
- **NVIDIA (HAMi-core).** `xbuild-nvidia-cuda-*` → `libvgpu.so`. Verifies artifacts/linking, single-card injection (`nvidia-smi` shows the sliced VRAM + the SM/compute limit), and **multi-card per-device limits** on real GPUs.

It is the build+runtime-contract counterpart to the cluster-level `gpustack-operator-e2e` (scheduling chain) and `gpustack-operator-chart-e2e` (chart). Evolving, e2e-style — extend the cases as the build flow grows.

## When to offer it

Proactively suggest this skill when a branch changes the Docker build flow:

```bash
git diff --name-only origin/main...HEAD | grep -E 'pack/gpustack-operator/(Dockerfile|external/(ascend|nvidia)/)'
```

## Runner model (local or remote)

All scripts source `scripts/lib.sh` and run through one runner, selected by env:
- `XB_MODE=local` — build & verify on this host.
- `XB_MODE=ssh XB_HOST=user@host` — build & verify on a remote host. Files move via base64-over-ssh (never scp — a login banner corrupts it); a remote login banner is filtered from output.

The remote host is **never hardcoded** — always ask the user for it.

## Hard rules

- **Never push images** — builds use `buildx --load` into the local/remote docker store only.
- **Confirm before any remote build or container run** (they consume the host's accelerator/driver). Preflight and the build-artifact case (ASCEND-CASE 1 / NVIDIA-CASE 1) are safe once the user names the target.
- **Touch only what the skill creates** — the `vcann-build:*` / `vgpu-build:*` image, `${XB_STAGE}` artifacts, `${XB_STAGE}/test` config/preload, the remote build context. Never modify the user's other resources.
- **Hardware cases require a real accelerator** (local or the ssh host): ASCEND-CASE 2/3 need an NPU; NVIDIA-CASE 2 needs a GPU, NVIDIA-CASE 3 needs **≥ 2** GPUs. The two CASE-1 builds need only docker+buildx.

## Flow

1. **Discover targets.** List the builder stages and ask which to verify (multi-select):
   ```bash
   grep -nE 'AS xbuild-(ascend-cann|nvidia-cuda)-' pack/gpustack-operator/Dockerfile
   ```
   Ascend: `xbuild-ascend-cann-8-910b`, `-8-910c`, `-9-910b`, `-9-910c`, `-9-950`. NVIDIA: `xbuild-nvidia-cuda-12`, `-13`.

2. **Pick connection (AskUserQuestion).** Local, or ssh — and if ssh, the host. Set `XB_MODE`/`XB_HOST`.

3. **Preflight (read-only, confirm target first).**
   ```bash
   XB_MODE=… XB_HOST=… bash .claude/skills/gpustack-operator-xbuild-and-verify/scripts/preflight.sh
   ```
   docker+buildx must PASS to build. The hardware rows (`npu-smi`/ascend-runtime/`/dev/davinci*` and `nvidia-smi`/nvidia-runtime/`/dev/nvidia*`) WARN when absent — the matching hardware cases are then unavailable. buildx missing → the table prints the install one-liner.

4. **Build the chosen target (confirm).** `build.sh` infers the backend from the target prefix; native on a matching-arch host (fast), cross-arch uses qemu.
   ```bash
   XB_MODE=… XB_HOST=… bash .claude/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh xbuild-nvidia-cuda-13
   ```
   Produces `XB_IMAGE` (Ascend `vcann-build:<suffix>` / NVIDIA `vgpu-build:<suffix>`) and stages the artifacts under `XB_STAGE` (Ascend `/opt/enpu/vcann-rt`, NVIDIA `/opt/vgpu`). The CANN/CUDA-based image doubles as the workload image for the hardware cases (`XB_WORKLOAD_IMAGE` defaults to it).

5. **Run cases.** Pass the same target; read each PASS/FAIL table — don't re-derive from raw logs.
   ```bash
   # Ascend
   XB_MODE=… XB_HOST=… bash .../cases/ascend-case-1.sh xbuild-ascend-cann-8-910b
   XB_MODE=… XB_HOST=… XB_NPU=0 bash .../cases/ascend-case-2.sh xbuild-ascend-cann-8-910b
   XB_MODE=… XB_HOST=… XB_NPU=0 XB_MEM=1024 bash .../cases/ascend-case-3.sh xbuild-ascend-cann-8-910b
   # NVIDIA
   XB_MODE=… XB_HOST=… bash .../cases/nvidia-case-1.sh xbuild-nvidia-cuda-13
   XB_MODE=… XB_HOST=… XB_GPU=0  XB_MEM=4096 XB_SM=50 bash .../cases/nvidia-case-2.sh xbuild-nvidia-cuda-13
   XB_MODE=… XB_HOST=… XB_GPUS=0,1 XB_MEM=4096       bash .../cases/nvidia-case-3.sh xbuild-nvidia-cuda-13
   ```

## Cases (locked titles)

### Ascend (`xbuild-ascend-cann-*`)
| Case | Title | Needs NPU | Asserts |
|---|---|---|---|
| 1 | Build artifacts + linking | no | `libvruntime.so` (0644) + `enpu-monitor` (0755) exist; ELF arch == build platform; build linked (the `--allow-shlib-undefined` path); both `NEEDED` `libc_sec.so`+`libascendcl.so`; notes the weak UND `dcmi_*` syms |
| 2 | Inject + enpu-monitor | yes | VDie-ID→`shm-id`; render `npu_info.config`; preload (libdcmi×2 + libvruntime); container `enpu-monitor` loads all 6 fields, initializes, and prints `Aicore Limit Quota`/`Memory Limit quota` matching the config |
| 3 | Memory-quota enforcement | yes | injected HBM alloc capped at `memory-quota` (the `Out of memory! … quota:<bytes>` log); baseline (no inject) exceeds it |

### NVIDIA (`xbuild-nvidia-cuda-*`)
| Case | Title | Needs GPU | Asserts |
|---|---|---|---|
| 1 | Build artifacts + linking | no | `libvgpu.so` (0644) exists; ELF arch == build platform; `NEEDED` `libcuda.so.1`+`libnvidia-ml.so.1` (hard deps the NVIDIA runtime injects — no weak-UND preload, contrast Ascend) |
| 2 | Single-card inject + nvidia-smi | yes (1 GPU) | preload `libvgpu.so`; with `CUDA_DEVICE_MEMORY_LIMIT_0`+`CUDA_DEVICE_SM_LIMIT`, `nvidia-smi memory.total` == the limit (NVML hook); a CUDA probe logs `core utilization limit = <SM>` and `cuMemGetInfo` total == the limit (real CUDA-level enforcement) |
| 3 | Multi-card per-device limits | yes (≥2 GPU) | each exposed card gets a **distinct** `CUDA_DEVICE_MEMORY_LIMIT_<i>` and the container's `nvidia-smi` reports each card's `memory.total` at its own limit (skips with WARN if too few GPUs) |

## Env knobs
`XB_MODE`/`XB_HOST` (runner); `XB_PLATFORM` (default from target arch); `XB_IMAGE`/`XB_WORKLOAD_IMAGE`;
`XB_STAGE` (Ascend `/opt/enpu/vcann-rt` | NVIDIA `/opt/vgpu`); `XB_REMOTE_CTX` (remote build-context dir).
- Ascend: `XB_NPU`/`XB_CHIP` (card/chip, default 0); `XB_VNPU` (0); `XB_AICORE` (20); `XB_MEM` (1024 MB).
- NVIDIA: `XB_GPU` (single-card index, 0); `XB_GPUS` (multi-card csv, `0,1`); `XB_MEM` (MiB, 4096);
  `XB_SM` (compute %, 50 / 30).

## References
- `references/ascend-npu-info-config.md` — Ascend: the 6 config fields, VDie-ID→shm-id, allocator mapping.
- `references/ascend-ld-preload-and-libdcmi.md` — Ascend activation via `/etc/ld.so.preload`; **why libdcmi must
  be preloaded** (weak dcmi syms); the `libc_sec`/CANN-image requirement.
- `references/ascend-npu-smi-and-aicore.md` — Ascend: `npu-smi` shows the physical card; AICore-quota mechanism,
  the benign CANN-8.5.0 warnings, the unverified-throttle gap.
- `references/nvidia-hami-core-vgpu.md` — NVIDIA: what `libvgpu.so` is, the env+mount injection contract, the
  one-CUDA-major-per-container rule, HAMi-core knobs.
- `references/nvidia-smi-and-sm-limit.md` — NVIDIA: memory limit is directly visible in `nvidia-smi` (NVML
  hook); the SM/compute limit is a time-slice throttle (HAMi log / under-load only); CUDA-13 probe gotchas.
- `references/troubleshooting.md` — both backends: scp banner, buildx-missing, Ascend link/segfault/hgemm,
  NVIDIA runtime/preload/stale-cache/cuCtxCreate-v4/stub-lib/SM-visibility.
