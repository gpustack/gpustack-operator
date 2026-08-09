---
name: gpustack-operator-xbuild-and-verify
description: "Build and verify the GPUStack Operator's accelerator logical-slicing **builder stages** (`xbuild-ascend-cann-*` and `xbuild-nvidia-cuda-*` in `pack/gpustack-operator/Dockerfile`) end to end, either on the local docker host or on a remote accelerator host over ssh. Builds one stage via buildx `--target`, then runs numbered cases against the produced runtime. SCOPE — four backends: **Ascend (vcann-rt: `libvruntime.so` + `enpu-monitor`)**, **NVIDIA (HAMi-core: `libvgpu.so`)**, **THead PPU (the `csrc/thead/ppu-slicing-shim/` slicing shims)** and **AMD ROCm (the `csrc/amd/rocm-slicing-shim/` slicing shim: `libvrocm.so` + `rocm-monitor` + `rocm-cumask-check`)**. Ascend cases: (1) artifacts+linking [no NPU], (2) inject + `enpu-monitor`, (3) memory-quota enforcement, (4) `npu-smi` slice visibility. NVIDIA cases: (1) artifacts+linking [no GPU], (2) single-card inject + `nvidia-smi`/SM-limit, (3) multi-card per-device limits. THead cases: (1) shim build+linkage [no PPU], (2) `ppu-smi` visibility via the `dlsym` hook, (3) driver-layer memory-path quota, (4) per-process utilisation, (5) per-card quota independence, (6) `common/` unit tests, one quota across two processes and one container across two cards, (7) compute throttling under a saturating load. AMD cases: (1) shim build+linkage [no GPU], (2) single-card inject + all three reported-capacity entry points, (3) memory-path completeness incl. the pool family, (4) CU-mask conformance and every fail-open construction, (5) compute quota semantics, (6) `common/` unit tests + one quota two processes + one container two cards, (7) `SIGKILL` reclaim and cross-ROCm-version reach. The hardware cases need a real accelerator; a THead host needs no docker, `nerdctl` is enough, and an AMD target that is itself a container needs no runtime at all. Proactively offer this whenever a branch changes the Docker build flow or the slicing shims — `pack/gpustack-operator/Dockerfile`, `pack/gpustack-operator/external/(ascend|nvidia)/**`, `pack/thead-ppu-devel/**`, `csrc/thead/**` or `csrc/amd/**`. Examples: \"verify my Dockerfile build-stage change\", \"did the vcann-rt / HAMi-core build still link\", \"test the logical-slicing build on the 910B / 4090 host\", \"does enpu-monitor still work in a container\", \"does nvidia-smi show the sliced memory\", \"why doesn't npu-smi show the Ascend slice\", \"does ppu-smi show the PPU slice\", \"run the THead slicing gates on the PPU host\", \"run the AMD slicing gates on the ROCm host\", \"did the CU mask actually take effect\", \"prove the memory slice is enforced on real hardware\"."
allowed-tools: "Read, AskUserQuestion, Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/scripts/preflight.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/ascend-case-1.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/ascend-case-2.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/ascend-case-3.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/ascend-case-4.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-1.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-2.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-3.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-1.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-2.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-3.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-4.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-5.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-6.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/thead-case-7.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/amd-case-1.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/amd-case-2.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/amd-case-3.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/amd-case-4.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/amd-case-5.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/amd-case-6.sh*), Bash(bash .claude/skills/gpustack-operator-xbuild-and-verify/cases/amd-case-7.sh*), Bash(grep*), Bash(git diff*), Bash(git rev-parse*), Bash(ssh*), Bash(docker buildx*), Bash(docker images*), Bash(docker info*), Bash(nerdctl images*), Bash(nerdctl namespace*), Bash(crictl images*), Bash(crictl pull*), Bash(command -v*)"
model: sonnet
---

# GPUStack Operator — accelerator xbuild & verify

Build one logical-slicing builder stage from `pack/gpustack-operator/Dockerfile` and verify the runtime it produces, on the local docker host or on a remote accelerator host over ssh. Four backends:

- **Ascend (vcann-rt).** `xbuild-ascend-cann-*` → `libvruntime.so` + `enpu-monitor`. Verifies artifacts/linking, the `npu_info.config` injection, and **real memory-quota enforcement** on a real NPU.
- **NVIDIA (HAMi-core).** `xbuild-nvidia-cuda-*` → `libvgpu.so`. Verifies artifacts/linking, single-card injection (`nvidia-smi` shows the sliced VRAM + the SM/compute limit), and **multi-card per-device limits** on real GPUs.
- **AMD ROCm (the `libvrocm.so` shim).** `build.sh xbuild-amd-rocm` stages `csrc/amd/rocm-slicing-shim/` onto the target and compiles it **inside** a ROCm devel image → `libvrocm.so` + `rocm-monitor` + `rocm-cumask-check` + the four gate binaries. Same in-repo-source shape as THead: the source is in this repo, so there is no upstream commit to pin, nothing to fetch and no builder stage in the Dockerfile; the recipes live in `csrc/amd/rocm-slicing-shim/build.sh`, which this arm calls. One difference belongs to the target rather than the backend — a rented single-card instance **is** a container, with ROCm on its filesystem and no container runtime at all, so when nothing resolves and `hipcc` is present the arm compiles **in place**. Verifies linkage, the reported-capacity surface, memory-path completeness including the pool family, **CU-mask conformance against the table the card's `NUM_XCC` selects**, compute semantics, and cross-ROCm-version reach.
- **THead PPU (the `libvppu.so` shims).** `build.sh xbuild-thead-ppu` stages `csrc/thead/ppu-slicing-shim/` onto the target and compiles it **inside** the published `gpustack/thead-ppu-devel:2.1.1` SDK image — that image is where `hggc.h` lives, and the Dockerfile has no builder stage for this backend yet. The recipes are not in the skill: the shim tree owns them in its own `build.sh`, which this arm calls; `csrc/thead/ppu-slicing-shim/README.md` covers building and using the shims outside verification. The cases then inspect and run what it produced, verifying the four feasibility gates on a real PPU. This backend is the one that runs on a host with **no docker**: everything here only `run`s containers, so `nerdctl` is enough and no buildkitd is started.

It is the build+runtime-contract counterpart to the cluster-level `gpustack-operator-e2e` (scheduling chain) and `gpustack-operator-chart-e2e` (chart). Evolving, e2e-style — extend the cases as the build flow grows.

## When to offer it

Proactively suggest this skill when a branch changes the Docker build flow or a slicing shim:

```bash
git diff --name-only origin/main...HEAD | grep -E 'pack/gpustack-operator/(Dockerfile|external/(ascend|nvidia)/)|csrc/(thead|amd)/'
```

## Runner model (local or remote)

All scripts source `scripts/lib.sh` and run through one runner, selected by env. The same case logic runs against every mode, which is what lets one script cover a docker laptop, a production accelerator node and a rented instance behind a proxy:
- `XB_MODE=local` — build & verify on this host.
- `XB_MODE=ssh XB_HOST=user@host` — build & verify on a remote host. Files move via base64-over-ssh (never scp — a login banner corrupts it); a remote login banner is filtered from output.
- `XB_MODE=pty XB_HOST=user@proxy` — a target whose SSH offers an **interactive shell and nothing else**: `ssh host '<cmd>'` connects and runs nothing, and scp/sftp are absent. Rented single-card instances are usually this shape. The script is typed on stdin and the answer comes back as one base64 line, because the stream also carries the shell's prompt and the terminal's own echo of what was sent — measured on such a target, the echo arrives three times and character-corrupted at the terminal width, so nothing looser survives it. Files are typed in `XB_PTY_CHUNK`-sized pieces and **verified by digest**, since a line discipline drops what overflows its buffer without saying so.
- `XB_SSH_OPTS` — extra ssh options, word-split. A target reachable only through a bastion needs `-J user@bastion` here, otherwise every hardware case has to be run by hand outside the skill.
- `XB_CTR` / `XB_CTR_ARGS` — the container runtime **on the target**, probed as `docker` then `nerdctl`. A caller on a docker laptop can drive a host that only has `nerdctl`; on a k3s/rke2 host the cluster's images live in another containerd namespace, so set `XB_CTR_ARGS='--namespace k8s.io'`. `XB_CTR=none` forces the AMD arm's in-place route on a target that has both.

The remote host is **never hardcoded** — always ask the user for it.

## Hard rules

- **Never push images** — builds use `buildx --load` into the local/remote docker store only.
- **Confirm before any remote build or container run** (they consume the host's accelerator/driver). Preflight and the build-artifact case (ASCEND-CASE 1 / NVIDIA-CASE 1) are safe once the user names the target.
- **Touch only what the skill creates** — the `vcann-build:*` / `vgpu-build:*` image, `${XB_STAGE}` artifacts, `${XB_STAGE}/test` config/preload, the remote build context. Never modify the user's other resources.
- **Hardware cases require a real accelerator** (local or the ssh host): ASCEND-CASE 2/3/4 need an NPU; NVIDIA-CASE 2 needs a GPU, NVIDIA-CASE 3 needs **≥ 2** GPUs; THEAD-CASE 2/3/4 need a PPU, and THEAD-CASE 5 plus THEAD-CASE 6's Part C need **≥ 2 idle** ones; AMD-CASE 2..7 need an AMD GPU, and AMD-CASE 6's two-card row needs **≥ 2**. The two CASE-1 builds need docker+buildx; THEAD-CASE 1 needs only a runtime that can `run`, and AMD-CASE 1 needs neither when the target already carries ROCm.
- **Run the AMD cases against both an RDNA and a CDNA host when one is available** — a green suite on one architecture says nothing about the other. AMD-CASE 4/5 read `NUM_XCC` and assert a *different* conformance table and a *different* fail-open set on each; the two derivations share no arithmetic. `preflight.sh` reports `NUM_XCC` before anything is built precisely so the choice is visible up front. A single-architecture run is a partial result and must be reported as one. Note what a runtime-less target can and cannot do, because multi-XCC hardware is rentable only as an instance that **is itself a container**: **AMD-CASE 4/5 need only a card**, since they drive staged binaries under `HSA_CU_MASK`. **AMD-CASE 2/3 reach such a target in place** — with no runtime they write `/etc/ld.so.preload` on the instance's own root filesystem, which is the same mechanism by another owner, and they refuse to start if the target already carries one of its own. **AMD-CASE 6/7 split**: the arms that link no ROCm — case 6's unit suite, case 7's lifecycle — run anywhere, and the arms that need something the instance cannot provide skip and say which thing. Case 7's cross-version arm needs two ROCm majors and therefore two images; case 6's cross-process arms need a container, and its per-card keying arm a second card.
- **Never start buildkitd on a user-owned host, and never install docker there** — a PPU/NPU host is usually someone else's production node. `preflight.sh` reports a missing `docker buildx` as a build-capable **WARN**: build the image on a docker host and load it. Only a missing *run* capability is a FAIL.
- **Never pick a THead card by index** — the PPU test host runs production inference and a card can already hold ~91 GB. `lib.sh: thead_idle_cards` reads idle cards out of `ppu-smi`'s own table; `XB_PPU_CARD`/`XB_PPU_CARDS` override it only when the user names one.
- **A case never decides its own verdict** — it ends with `xb_verdict "<LABEL>" "$(xb_fails "${out}")"`, and its payload prints `FAILS=<n>` as a whole line of its own. What that helper replaced grepped the output for the token `FAILS=0` anywhere, which any row could satisfy by printing it in a detail column — AMD-CASE 4 did, so the case that exists to catch a silently discarded CU mask could not itself fail: measured, one deliberately broken assertion printed a red row, printed `FAILS=1`, and still exited 0 saying PASS. `preflight.sh` FAILs if any case under `cases/` decides for itself, and checks the helper's own arithmetic on two inputs, because 21 cases calling one wrong function go green together.
- **Never write the *host's* `/etc/ld.so.preload`** — every preload the cases install is scoped to a container (ASCEND-CASE 4 builds and preloads a throwaway dsmi interposer as a mechanism control; that shim is a probe, never product code — the shipped dsmi hook is the vendored patch in `pack/gpustack-operator/external/ascend/vcann-rt/`).

## Flow

1. **Discover targets.** List the builder stages and ask which to verify (multi-select):
   ```bash
   grep -nE 'AS xbuild-(ascend-cann|nvidia-cuda)-' pack/gpustack-operator/Dockerfile
   ```
   Ascend: `xbuild-ascend-cann-8-910b`, `-8-910c`, `-9-910b`, `-9-910c`, `-9-950`. NVIDIA: `xbuild-nvidia-cuda-12`, `-13`. The two in-repo-source backends have **no** Dockerfile stage and so do not appear in that grep — `xbuild-thead-ppu` and `xbuild-amd-rocm` are targets of `build.sh` alone.

2. **Pick connection (AskUserQuestion).** Local, or ssh — and if ssh, the host. Set `XB_MODE`/`XB_HOST`.

3. **Preflight (read-only, confirm target first).**
   ```bash
   XB_MODE=… XB_HOST=… bash .claude/skills/gpustack-operator-xbuild-and-verify/scripts/preflight.sh
   ```
   The gate has two halves: **run-capable** is the only FAIL, because nothing can execute a case without it — a runtime that answers is a PASS, no runtime but an in-place ROCm toolchain is a **WARN** (the AMD arm works there, no other backend does), neither is the FAIL; **build-capable** (`docker buildx`) is a WARN naming the build-elsewhere-then-load route. The hardware rows (`npu-smi`/ascend-runtime/`/dev/davinci*`, `nvidia-smi`/nvidia-runtime/`/dev/nvidia*`, `ppu-smi`/`/dev/alixpu*`/the `alixpu` module, `rocm-smi`/`/dev/kfd`/the `amdgpu` module) WARN when absent — the matching hardware cases are then unavailable. The fourth AMD row reports **`NUM_XCC`**, which is not a capability but the value that decides *which* conformance table AMD-CASE 4/5 assert; it is read straight out of the KFD topology, the one figure in that directory safe to take at face value, since the neighbouring `array_count` has already been multiplied by it. To exercise the failure path: `XB_CTR=nosuchruntime` gives exactly one FAIL and `FAILS=1` on a host with no in-place ROCm.

4. **Build the chosen target (confirm).** `build.sh` infers the backend from the target prefix; native on a matching-arch host (fast), cross-arch uses qemu.
   ```bash
   XB_MODE=… XB_HOST=… bash .claude/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh xbuild-nvidia-cuda-13
   ```
   For Ascend it also ships `external/ascend/vcann-rt/*.patch` into the build context, because every `xbuild-ascend-cann-*` stage bind-mounts that directory and `build-libvnpu.sh` applies it to the pinned upstream source — a build that cannot find it fails rather than shipping an unpatched library.
   Produces `XB_IMAGE` (Ascend `vcann-build:<suffix>` / NVIDIA `vgpu-build:<suffix>`) and stages the artifacts under `XB_STAGE` (Ascend `/opt/enpu/vcann-rt`, NVIDIA `/opt/vgpu`). The CANN/CUDA-based image doubles as the workload image for the hardware cases (`XB_WORKLOAD_IMAGE` defaults to it).

5. **Run cases.** Pass the same target; read each PASS/FAIL table — don't re-derive from raw logs.
   ```bash
   # Ascend
   XB_MODE=… XB_HOST=… bash .../cases/ascend-case-1.sh xbuild-ascend-cann-8-910b
   XB_MODE=… XB_HOST=… XB_NPU=0 bash .../cases/ascend-case-2.sh xbuild-ascend-cann-8-910b
   XB_MODE=… XB_HOST=… XB_NPU=0 XB_MEM=1024 bash .../cases/ascend-case-3.sh xbuild-ascend-cann-8-910b
   XB_MODE=… XB_HOST=… XB_NPU=0 XB_MEM=1024 bash .../cases/ascend-case-4.sh xbuild-ascend-cann-8-910b
   # NVIDIA
   XB_MODE=… XB_HOST=… bash .../cases/nvidia-case-1.sh xbuild-nvidia-cuda-13
   XB_MODE=… XB_HOST=… XB_GPU=0  XB_MEM=4096 XB_SM=50 bash .../cases/nvidia-case-2.sh xbuild-nvidia-cuda-13
   XB_MODE=… XB_HOST=… XB_GPUS=0,1 XB_MEM=4096       bash .../cases/nvidia-case-3.sh xbuild-nvidia-cuda-13
   ```

   **THead has a target like the others, built differently** — `xbuild-thead-ppu` compiles the shim tree inside the published SDK image with `run` rather than building a Dockerfile stage with buildx, because no such stage exists yet and the PPU host has no docker. Run it first: every case consumes what it staged, so **a source edit needs it re-run**.
   ```bash
   XB_MODE=ssh XB_HOST=… XB_CTR=nerdctl XB_CTR_ARGS='--namespace k8s.io' \
     bash .../scripts/build.sh xbuild-thead-ppu                     # no PPU; stages + compiles
   bash .../cases/thead-case-1.sh                                   # no PPU; build + linkage
   XB_MODE=ssh XB_HOST=… XB_SSH_OPTS='-J user@bastion' \
     XB_CTR=nerdctl XB_CTR_ARGS='--namespace k8s.io' \
     bash .../cases/thead-case-2.sh                                 # Gate 1
   … thead-case-3.sh   # Gate 2      … thead-case-4.sh   # Gate 3      … thead-case-5.sh   # two cards
   … thead-case-6.sh   # common/ unit tests (no PPU) + one quota two processes + one container two cards
   … thead-case-7.sh   # Gate 4: compute throttling under a saturating kernel load
   ```
   The image has to be present on the target. `nerdctl pull` does **not** read containerd's `config.toml` mirrors, so on a host where `docker.io` times out, pull through CRI — which does use the cluster's configured mirrors — and then run with `nerdctl`:
   ```bash
   crictl pull docker.io/gpustack/thead-ppu-devel:2.1.1     # lands in the k8s.io namespace
   ```

   **AMD works the same way** — `xbuild-amd-rocm` first, then the cases consume what it staged, so **a source edit needs it re-run**.
   ```bash
   XB_MODE=ssh XB_HOST=… bash .../scripts/build.sh xbuild-amd-rocm    # in a ROCm devel image
   XB_MODE=pty XB_HOST=… bash .../scripts/build.sh xbuild-amd-rocm    # a PTY-only instance that
                                                                     # IS a container: in place
   bash .../cases/amd-case-1.sh                                       # no GPU; build + linkage
   XB_MODE=… XB_HOST=… XB_AMD_GPU=0 XB_AMD_QUOTA_MIB=4096 bash .../cases/amd-case-2.sh
   … amd-case-3.sh   # memory paths incl. the pool family
   … amd-case-4.sh   # CU-mask conformance — the table follows NUM_XCC
   … amd-case-5.sh   # compute semantics: single-tenant ceiling AND multi-tenant sharing
   … amd-case-6.sh   # common/ unit tests + one quota two processes + one container two cards
   … amd-case-7.sh   # SIGKILL reclaim + the same artifact under ROCm 7.x and 6.x
   ```

## Cases (locked titles)

### Ascend (`xbuild-ascend-cann-*`)
| Case | Title | Needs NPU | Asserts |
|---|---|---|---|
| 1 | Build artifacts + linking | no | `libvruntime.so` (0644) + `enpu-monitor` (0755) exist; ELF arch == build platform; build linked (the `--allow-shlib-undefined` path); both `NEEDED` `libc_sec.so` and **not** `libascendcl.so` (upstream dropped it at ubs-virt `476bb968` for a runtime `dlopen` — asserted in the negative as a revert tripwire); `libvruntime.so` defines the rt-layer interposition surface (80 rt-prefixed FUNCs, incl. 15 lowercase-s `rts*`) and **no** `dcmi_*` definition, i.e. dcmi client not interposer; it defines **exactly one** `dsmi_*`, `dsmi_get_hbm_info` — the vendored `external/ascend/vcann-rt/` patch, so a patch that stopped applying fails here instead of shipping silently; both carry weak UND `dcmi_*` syms (why libdcmi must be preloaded) |
| 2 | Inject + enpu-monitor | yes | VDie-ID→`shm-id`; render `npu_info.config`; preload (libdcmi×2 + libvruntime); container `enpu-monitor` loads all 6 fields, initializes, and prints `Aicore Limit Quota`/`Memory Limit quota` matching the config |
| 3 | Memory-quota enforcement | yes | injected HBM alloc capped at `memory-quota` (the `Out of memory! … quota:<bytes>` log); baseline (no inject) exceeds it |
| 4 | npu-smi slice visibility | yes | `npu-smi` links `libdrvdsmi_host.so` and **neither** `libruntime.so` nor `libdcmi.so`, imports no `rt*` symbol, and is not setuid — so the rt-layer hooks cannot reach it; the shipped gate, both halves: `ENPU_DSMI_HOOK` unset ⇒ `npu-smi` shows the physical card, `=1` ⇒ it shows the quota, with `enpu-monitor` and the plain non-CANN binaries unaffected either way; a throwaway **dsmi** interposer halves `npu-smi`'s HBM as the mechanism control (`rewritten`); `hami-vnpu-core`, if staged at `XB_HAMI`, still has the blind spot while its hooked `rtMemGetInfoEx` reports the quota and enforces it (rows WARN-skip when unstaged) |

### NVIDIA (`xbuild-nvidia-cuda-*`)
| Case | Title | Needs GPU | Asserts |
|---|---|---|---|
| 1 | Build artifacts + linking | no | `libvgpu.so` (0644) exists; ELF arch == build platform; `NEEDED` `libcuda.so.1`+`libnvidia-ml.so.1` (hard deps the NVIDIA runtime injects — no weak-UND preload, contrast Ascend) |
| 2 | Single-card inject + nvidia-smi | yes (1 GPU) | preload `libvgpu.so`; with `CUDA_DEVICE_MEMORY_LIMIT_0`+`CUDA_DEVICE_SM_LIMIT`, `nvidia-smi memory.total` == the limit (NVML hook); a CUDA probe logs `core utilization limit = <SM>` and `cuMemGetInfo` total == the limit (real CUDA-level enforcement) |
| 3 | Multi-card per-device limits | yes (≥2 GPU) | each exposed card gets a **distinct** `CUDA_DEVICE_MEMORY_LIMIT_<i>` and the container's `nvidia-smi` reports each card's `memory.total` at its own limit (skips with WARN if too few GPUs) |

### THead PPU (no builder stage; shims from `csrc/thead/ppu-slicing-shim/`)
| Case | Title | Needs PPU | Asserts |
|---|---|---|---|
| 1 | Shim build + linkage | no | assumes `build.sh xbuild-thead-ppu` ran, then re-invokes the shim tree's own `build.sh` for the rows that are claims about the build — it carries no compiler flags and no translation-unit list of its own, so a case cannot drift from what ships, and it records the units behind each artifact. The two product shims and Gate 1's two stand-ins — the `dlsym`-less control and the second interposer case 2 stacks against the hook — compile with **no** diagnostics (not merely exit 0), judged on empty output because that script is silent when it succeeds; `DT_NEEDED` empty or exactly `libc.so.6`; highest `GLIBC_` requirement ≤ 2.17 (the SDK's floor, not the base-image tag); each artifact **defines** the symbols it interposes and **not** the ones it must not — `GLOBAL DEFAULT` with a non-`UND` section via `readelf -W --dyn-syms`, because `nm -D \| grep` also matches the *imported* `dlsym` and would pass a library that only calls it; the constructor's load marker is present; `hggc_quota` carries both the `DENIED` marker and the counter line, and all 54 names its module interposes are exported — 38 on the memory surface plus the 16 launch entries the compute cap is spent through — while the module's own `VPPU_INTERNAL` seam is not. Two string rows are a pair rather than two checks: the object names `libhgml.so` and `hgmlDeviceGetProcessUtilization` while `DT_NEEDED` above says it links neither, which is the only evidence the compute loop reads utilisation at **runtime** — a shim that linked it would pass the strings and fail `DT_NEEDED`, and one with no feedback at all would pass `DT_NEEDED` and lack the strings. One row is a compile rather than an inspection (`build.sh check v1`): a syntax-only pass with `__HGGC_API_VERSION_INTERNAL` + `__HGGC_API_VERSION_UMD` defined, so `hggc.h` itself type-checks the v1 prototypes its own plain-name mapping otherwise hides. The `tools/` reader is judged differently, being preloaded into nothing: the same linkage and glibc floor (it is mounted into the same containers), compiled with **no** SDK include path at all, and then the claim that carries the contract — it reads a usage region this case writes with `dd` from the offsets in `references/thead-usage-region.md`, never from a header in the tree, so a field looked for in the wrong place cannot be hidden by writer and reader agreeing with each other; card 3 is in that region for the 576-byte stride and a process slot for the offset every other reader hard-codes. It must also **refuse** a bumped layout version rather than misparse it, and report an absent region as its own outcome (exit 1) rather than as a corrupt one (exit 2) — an unsliced container is not a broken ledger. Records each staged path + `sha256` for the later cases |
| 2 | Gate 1: ppu-smi slice visibility | mechanism rows no, visibility rows yes | **without hardware:** `dladdr` on the pointer `dlsym` returns names the winning object — the hook puts both memory getters inside the shim, the control leaves them inside `libhgml.so` (defining the HGML symbols alone is inert), and the control proves its own load. A second `dlsym` interposer is then stacked beside the hook in **both** preload orders: the front one wins and is the **only** one in the chain — two libraries interposing `dlsym` with a versioned `dlvsym` step over each other rather than chaining — each proved loaded so "stepped over" cannot be confused with "never loaded", and no lookup hands a peer a pointer into the hook. **With a card:** a measured baseline (never the literal 98304), arm (a) hook → memory field equals the quota **exactly** and the interception marker is present, arm (b) control → equals the baseline **and** proves it loaded, arm (c) plus the vendor `libhggc_wrapper.so` → no recursion or deadlock, timeout-bounded, and the wrapper **proves it loaded** too (`LD_DEBUG=libs` naming its `calling init:`), because an absent one leaves the hook working alone and the row would report the quota for the wrong reason; not loadable at all is a SKIP, not a PASS. Arms (d)/(e) are the same pair at call time and expect **different** figures — the quota with the hook in front, the physical card with the peer in front, which is the ordering constraint on the injection contract pinned as a fact. Arm (f) is the `used` side: one process spends half the quota under the enforcement shim and holds it, and `ppu-smi` under the visibility shim must report **that** figure, which it can only have read out of the shared ledger. Every row decided by parsed output: `ppu-smi` exits 0 even on `init HGML error` |
| 3 | Gate 2: memory-path completeness | yes | five paths (plain, async, pool, VMM, procaddr) × three observations: under quota with the shim succeeds; the same over-quota size succeeds **without** the shim (so the refusal is ours, not the platform's); over quota with the shim is refused **carrying the `DENIED` marker**. A fourth row reads the shim's per-entry counter, the only evidence the call crossed `libhggc.so`. `procaddr` is the one path that does not reach an entry by name: it asks `hgGetProcAddress` for the driver's own address and allocates through the returned pointer, which is how the runtime layer binds what it needs and how a caller walks past the interposition of an entry point. A last row stands outside the paths: half the quota twice, both freed, then the whole quota — the only row that can see whether a **free refunds**, since every other one allocates once in a fresh process. Not-refused-and-no-counter-moved is reported as the shim watching the wrong ABI name **or** a bypass, never as a settled premise failure |
| 4 | Gate 3: per-process utilisation characterisation | yes | the staged probe runs under its own controlled load; `hgmlDeviceGetProcessUtilization` yields a concrete supported/`empty`/`others-only`/unsupported verdict — neither a success with zero samples nor one whose only samples belong to another process is support, the query asking for all history — and the reported `pid` is characterised as the container's or the host's by matching the probe's own, with the raw `NSpid` line printed. Decides whether the compute PID loop can be fed per-process utilisation |
| 5 | multi-card per-device quota independence | yes (≥2 idle) | two containers, two **distinct** idle cards and one quota each, run **concurrently** (sequential runs would pass even with shared accounting, and one index twice would not test independence at all); a size between the two quotas is refused by the smaller and served by the larger; the smaller container's `DENIED` marker must name **its own** quota figure, since the other's number is what a leaked ledger looks like |
| 6 | `common/` unit tests, one quota across two processes, one container across two cards | part A no, parts B/C yes (C needs **2 idle**) | **part A** builds `common/`'s unit tests with **no SDK header at all** — which is the point of the rule that `common/` names no `hg*`/`hggc*`/`hgml*` type — and relays their rows: quota parsing (unset, zero, malformed, trailing junk, overflow), the key map including the tombstone case a full table makes the common one, the region's magic/version/slot counts read back **by documented offset out of the raw file**, an unknown layout version and a foreign file both refused, two forked processes serialised on one card's lock, and a dead process's charge reclaimed. Its `FAILS=` line is folded into this case's count and **not** relayed, because the verdict reads the last such line. **Part B** puts two processes in **one** container against one figure: the first holds the whole quota, the second asks for 1MiB and must be refused carrying the `DENIED` marker — 1MiB because the card has tens of GiB free, so a refusal that small can only be ours — and `/dev/shm/vppu-ledger` must open with the documented magic. While that quota is held, the same figures are then read **three ways and required to agree**: the shim's own `DENIED` line, `tools/ppu-monitor` (run with `LD_PRELOAD` cleared, so it demonstrably reads the region rather than any shim symbol), and `od` at the documented offsets. The third is the point — a reader that only ever agreed with the struct it was compiled against would say nothing about the contract a scraper writes to — and the compute **limit** the reader prints appears in no `ppu-smi` field at all. The `od` lines are tagged rather than fenced between markers, because the holder's own `[vppu]` output arrives on the same merged stream and a range between markers parses log text as numbers. **Part C** is the only place in the suite where **per-card** keying can be observed: every other card row gives a container one card, so a shim that ignored the card index and charged one container-wide figure would pass all of them, case 5's two containers included. One container holds **two** cards with two different figures and a size **between** them is asked for on each — refused on the smaller naming that card's own quota in bytes, served on the larger — which one figure cannot answer both ways; the reader must then show both cards at their own figure, its first real multi-card region rather than the one case 1 writes with `dd`. Two further rows cover the entries that carry a card of their **own** — a VMM allocation whose `prop` names the other card, and an allocation from the other card's **default** pool, both issued from a context on the smaller card: each succeeds only if the charge follows the card it names rather than the calling thread's context (both verified to fail against the pre-fix shim); and `ppu-smi` inside that same two-card container must report **each card its own total**, which asks the keying question of the hgml shim's handle-to-index step — case 2 proves it reports a quota, but with one card a shim answering card 0's figure for every handle would pass it; and a second run of that container swaps the memory figures to the **un-indexed** form for one card
while the other keeps its own `_1`, so the fallback arm of the precedence is measured rather than assumed |
| 7 | Gate 4: compute throttling | yes | a kernel load built by **hgcc** (there is no way to occupy a PPU from plain C) runs back to back and reports its OWN per-process `smUtil`, never a card total, which could not tell one container's share from its neighbour's. Six rows: uncapped — with the limit at 100, the sharper control, so the cap is the only difference and the configured-but-uncapping path is exercised — the load pins the card; capped at `XB_PPU_SM_LIMIT` it settles inside a band **bounded on both sides** and at least 25 points below uncapped, because a container squeezed far under its cap is starved rather than capped (a card-total feedback signal settles it at about half, which a floor of "non-zero" would pass — measured); a **launch** counter moved, so the throttling is this shim's; the loop's own state — limit, measured utilisation, window, allowance, step, integral, error — reads back from the region **by documented offset out of the raw file**, since gains that are not fitted to this hardware can only be tuned on it if the loop is observable; two capped containers on ONE card each keep their own share, near their own cap; and a container with **no** compute figure is refused carrying the `DENIED` marker, which is the flip this task carries — before the controller existed a missing figure was reported and nothing more. Two further rows cover the cap being **per card** (needs **2 idle**): one container holds two cards at `XB_PPU_SM_LIMIT_A`/`_B`, a load runs on each **at the same time**, and each must settle in **its own** band — plus both figures read back from the region at their own card's offset (112 and 688), so one number copied into every slot cannot pass. They inject the indexed figures **without** the un-indexed one, so a shim that ignores the index finds no figure at all and refuses the container rather than quietly running both cards at one cap |

### AMD ROCm (no builder stage; the shim from `csrc/amd/rocm-slicing-shim/`)
| Case | Title | Needs GPU | Asserts |
|---|---|---|---|
| 1 | Shim build + linkage | no | assumes `build.sh xbuild-amd-rocm` ran, then re-invokes the shim tree's own `build.sh check` for the rows that are claims about the build — it carries no compiler flags and no translation-unit list of its own, so a case cannot drift from what ships. The four linkage assertions: `libvrocm.so` exports **only** the HIP names it interposes; `DT_NEEDED` is exactly `libc.so.6` — nothing ROCm, which is the whole basis of the claim that one build serves every ROCm version; no `GLIBC_` requirement above **2.4**, held by three `.symver` pins rather than by the base image's tag, so a current devel image cannot silently raise the floor that Ubuntu 20.04 / RHEL 8 workload images depend on; and **zero** `hip*`/`hsa*` symbols among the undefined ones. Each is checked with `readelf -W --dyn-syms` requiring `GLOBAL DEFAULT` with a non-`UND` section — never `nm -D \| grep`, which also matches an *imported* symbol and would pass for any library that merely calls it. The tree's four `sed` mutants are re-run, each of which must **fail its own named row** and no other, so an assertion that stopped asserting is caught rather than inherited. Everything compiles with **no** diagnostics, judged on empty output because that script is silent when it succeeds. Records each artifact's staged path and `sha256` so the later cases consume exactly what this one produced |
| 2 | Single-card inject + the reported-capacity surface | yes | preload `libvrocm.so`, set one card's quota, and read the capacity back through **all three** entry points plus `hipDeviceTotalMem`. The row that carries the finding is the **control**: an arm interposing only `hipMemGetInfo` must still show the *physical* figure through `hipDeviceProp_t.totalGlobalMem`, because ROCm 6+ headers rewrite `hipGetDeviceProperties` to `…R0600` and interposing the bare name lands on a symbol nothing calls. Without that control the full arm passing says nothing about which of the three did the work. `hip_props_probe` reports which object each name actually bound to (`dlsym` + `dladdr`), so "the quota was reported" and "our library reported it" are separate observations. `multiProcessorCount` is asserted **unchanged**, since memory and compute are independent surfaces here |
| 3 | Memory-path completeness | yes | every allocation family through its own entry — classic, managed, ext, pitch, 2D/3D array, host, stream-ordered async, and the **pool** family — one family per process so a refusal can only come from the size under test. The quota is deliberately set **below** one request: run with a quota larger than the request and freed between entries, the same table passes whether or not an entry is charged. `hipMallocFromPoolAsync` is the row that exists because it was measured bypassing a `hipMalloc`-level ledger entirely. Array families derive their shape from the device's own texture limits and report the ceiling, so a shape refusal (`hipErrorInvalidValue`) is never read as a quota refusal (`hipErrorOutOfMemory`). A last row stands outside the families: fill the quota with several allocations, free them all, re-request the whole quota — admitted only if every refund landed |
| 4 | CU-mask conformance | yes | drives `rocm-cumask-check` across **every row of the conformance table the card's `NUM_XCC` selects**, and across **every fail-open construction for that architecture** as *negative* rows — so a case set that stopped detecting them fails rather than quietly narrowing. The failures it guards are silent by construction: a rejected mask produces no error, no log line and no changed return code, the container simply gets the whole card. RDNA (`NUM_XCC=1`, unit = WGP): a mask splitting a WGP pair, a `GPU-<uuid>` device list, and `ROC_GLOBAL_CU_MASK` with a bit clear inside the valid width — all three measured occupying 30 of 30 WGPs. CDNA (`NUM_XCC>1`, unit = CU): a mask that leaves an XCC uncovered leaves it **unmasked**, measured at 267 of 304 CUs for `0:0` while throughput read a plausible 3.7 %; an out-of-range list; and a sub-atom request, which must be **rejected** rather than clamped. The probe re-execs itself with the mask set, because `HSA_CU_MASK` is read at ROCr init |
| 5 | Compute quota semantics | yes | the **single-tenant ceiling** first, then multi-tenant sharing — not aggregate alone. Measured on RDNA, a correct partition, a broken partition and no partition at all give indistinguishable concurrent readings; the difference appears only when one tenant runs alone. Measured on CDNA the harder case: two tenants sharing 152 CUs and two sharing none report the *same* per-tenant throughput, solo runs included, so on a multi-XCC card the row must additionally assert **occupancy** — the `HW_ID` readout, which is the only thing that tells them apart. Disjoint, fully-overlapping and mixed-capped rows. Every timed row uses `cumask_soak`'s cross-process **file barrier** and its **ILP-saturating** kernel: without the barrier N tenants report an aggregate above the card's physical peak, and a latency-bound kernel under-fills a small partition and inflates every overlap reading. Any PyTorch arm keeps warm-up outside the timed window — a first 8192² fp16 GEMM was measured spending over 400 s autotuning |
| 6 | `common/` unit tests, one quota across two processes, one container across two cards | part A no, parts B/C yes (C needs **2 cards**) | **part A** runs `common/`'s unit tests, which link **no ROCm at all** — that is the point of the rule that `common/` names no `hip*`/`hsa*` type — and relays their rows; its own `FAILS=` line is folded into this case's count and not relayed, because the verdict reads the last such line. **Part B** puts two processes in one container against one quota: the first holds it all, the second asks for a size small enough that only our refusal can explain it. The same figures are then read **two ways and required to agree** — the library's own denial, and `rocm-monitor`, run with the preload cleared so it demonstrably parses the usage region rather than calling any shim symbol. **Part C** is the only place **per-card** keying can be observed: every other row gives a container one card, so a shim charging one container-wide figure would pass all of them. One container holds two cards with two different quotas and a size **between** them is asked for on each — refused on the smaller naming that card's own figure, served on the larger — which one figure cannot answer both ways |
| 7 | Lifecycle and cross-version reach | yes | `ledger_lifecycle` takes a charge, is **`SIGKILL`ed** while holding it, and a later process with no memory of the first must reclaim it — measured before the sweep existed, a killed process holding 4 GiB of a 6 GiB quota left the next able to claim only 2, and the shortfall survived as long as the region file did. Then cross-version reach: the **same artifact** is exercised inside both a ROCm 7.x and a ROCm 6.x container, since the product links no ROCm object and one build is claimed to serve every version — the 6.x arm must enforce the same quota as the 7.x arm |

## Env knobs
`XB_MODE`/`XB_HOST`/`XB_SSH_OPTS` (runner); `XB_PTY_CHUNK` (pty mode, 1024);
`XB_CTR`/`XB_CTR_ARGS` (target container runtime; `XB_CTR=none` forces AMD's in-place route);
`XB_PLATFORM` (default from target arch); `XB_IMAGE`/`XB_WORKLOAD_IMAGE`;
`XB_STAGE` (Ascend `/opt/enpu/vcann-rt` | NVIDIA `/opt/vgpu` | THead `/tmp/vppu` | AMD `/tmp/vrocm`);
`XB_REMOTE_CTX` (remote build-context dir); `XB_ROCM_PATH` (AMD in-place ROCm prefix, `/opt/rocm`).
- Ascend: `XB_NPU`/`XB_CHIP` (card/chip, default 0); `XB_VNPU` (0); `XB_AICORE` (20); `XB_MEM` (1024 MB);
  `XB_HAMI` (case 4 only — where a `hami-vnpu-core` `libvnpu.so`/`libvnpu-needed.so` is staged for
  the comparison rows, default `/opt/hami-vnpu-core`; absent ⇒ those rows WARN-skip).
- NVIDIA: `XB_GPU` (single-card index, 0); `XB_GPUS` (multi-card csv, `0,1`); `XB_MEM` (MiB, 4096);
  `XB_SM` (compute %, 50 / 30).
- THead: `XB_PPU_CARD` / `XB_PPU_CARDS` (override the idle-card pick — normally leave unset);
  `XB_PPU_IDLE_MIB` (a card counts as idle at or below this used memory, 64); `XB_PPU_QUOTA_MIB` (4096),
  `XB_PPU_UNDER_MIB` (1024) and `XB_PPU_OVER_MIB` (8192) for the quota rows; `XB_PPU_QUOTA_A_MIB` (2048)
  and `XB_PPU_QUOTA_B_MIB` (6144) for case 5 **and case 6's Part C**, whose rows turn on a size midway between
  the two, so A must be the smaller (`XB_PPU_CARDS` picks Part C's pair the same way it picks case 5's); `XB_PROBE_ROUNDS` (case 4, 3); `XB_PPU_SM_LIMIT` (case 7,
  25), `XB_PPU_SM_LIMIT_A` / `XB_PPU_SM_LIMIT_B` (case 7's two-card arm, 50 / 25 — A must be the larger, since
  the rows turn on the two caps differing) and `XB_PPU_LOAD_SECONDS` (case 7, 20 — the loop needs several
  control steps to settle, so shortening this measures the cold start instead). Inside the container the cases
  set the shims' own contract: `HGGC_DEVICE_MEMORY_LIMIT_<i>` / `HGGC_DEVICE_MEMORY_LIMIT` (MiB, **per card**
  with the un-indexed figure covering every card carrying none of its own; `<i>` is the container-local index —
  `0` wherever a container gets one card node, `0` and `1` in the arms that pass two, since the SDK renumbers
  from 0 whatever the host ordinals are; case 6 Part C is the only place the un-indexed form is exercised),
  `HGGC_DEVICE_SM_LIMIT_<i>` / `HGGC_DEVICE_SM_LIMIT` (percent, **enforced** — the indexed figure decides its
  card wherever it is **set**, every other card reads the un-indexed one, and a figure that is set and
  malformed denies that card rather than falling through; an init error when absent, so every case that
  injects the shim injects it too; the memory cases pass the un-indexed `100`, which is configured and
  uncapping, and case 7's two-card arm is the only place the indexed form is exercised), and
  `LIBHGGC_LOG_LEVEL=2` — the cases pin `2`
  because the load markers and the counter dump they grep sit above the default level of `1`, which carries
  denials only. `HGGC_LEDGER_PATH` (the cross-process usage region, default `/dev/shm/vppu-ledger`) is left at
  its default by every case except the unit tests and case 7, which point each run at its own file: case 7's
  has to outlive the container so the loop state can be read back, and for the unit tests the region is
  the region is created by the first process that allocates and shared by the rest, so two cases sharing one
  would decide each other's rows. The compute controller's own knobs — `HGGC_SM_CONTROL_PERIOD_MS` (the gating
  window, 100), `HGGC_SM_CONTROL_STEP_MS` (how often the loop steps, 1000 — the driver's utilisation figure
  moves at about ten points per 100 ms, so a faster loop acts on a stale figure and oscillates),
  `HGGC_SM_CONTROL_KP`/`_KI`/`_KD` (gains ×100: 25/8/0) and `HGGC_SM_GRAPH_WEIGHT` (1, off) — are left at
  their defaults by every case, which is what makes case 7 a test of those defaults.
- AMD: `XB_AMD_GPU` (single-card index, 0) / `XB_AMD_GPUS` (multi-card csv, `0,1` — case 6's Part C);
  `XB_AMD_QUOTA_MIB` (4096), `XB_AMD_UNDER_MIB` (1024) and `XB_AMD_OVER_MIB` (8192) for the quota rows;
  `XB_AMD_QUOTA_A_MIB` (2048) / `XB_AMD_QUOTA_B_MIB` (6144) for the two-card row, whose rows turn on a size
  midway between them, so A must be the smaller; `XB_AMD_CU_PERCENT` (case 5's compute share, 50) and
  `XB_AMD_SOAK_SECONDS` (case 5's timed window, 20 — the barrier plus a saturating kernel need long enough
  to be measuring steady state rather than the launch); `XB_AMD_ROCM_OLD_IMAGE`
  (`rocm/dev-ubuntu-22.04:6.4.4`, case 7's second container — the point of that arm is that it is the **same**
  artifact, so the image is the only thing that changes). Note that **no** case sets an offload architecture:
  the tree's `build.sh` names them explicitly rather than letting `hipcc` detect one, because detection
  targets the *build* host's card and falls back silently on a host with none.
  Inside the container the cases set the shim's own contract, and **all three parts of it together** —
  `ROCR_VISIBLE_DEVICES`, `HSA_CU_MASK` and `VROCM_DEVICE_MEMORY_LIMIT_<i>` (MiB, per card) — because the
  `<i>` in the last two is a position in the **`ROCR_VISIBLE_DEVICES` list**, not a physical ordinal:
  measured, with `ROCR_VISIBLE_DEVICES=1,0` the `0:` segment addresses physical card 1. Changing any one of
  the three alone silently misaligns the other two. Also `VROCM_LEDGER_PATH` (the cross-process usage region;
  each case points its own run at its own file, since the region is created by the first process that
  allocates and shared by the rest, so two cases sharing one would decide each other's rows) and
  `LIBVROCM_LOG_LEVEL` (`2` — the caller-origin diagnostics the cases grep sit above the default). The mask
  variables are **fail-open** where the visibility ones are fail-closed, which is why case 4 exists at all.

## References
- `csrc/amd/rocm-slicing-shim/README.md` — AMD: how to build the shim, the two readers
  (`build.sh lib|tool|test|unit|check|list`), how to run `rocm-monitor` and `rocm-cumask-check` by hand, and
  the whole environment contract. **The build recipes live there, not here.** It also states the separation
  the cases depend on: computing a mask, injecting it, enforcing it and checking it are four different jobs
  in four different places, and only the last one lives in this tree.
- `references/amd-cumask-conformance.md` — AMD: both CU-mask derivations (RDNA by WGP pair, CDNA by XCC
  atom), both conformance tables with **measured** occupancy, every fail-open construction as a negative row,
  the `HW_ID`/`HW_ID1`/`XCC_ID` register layouts the occupancy readout decodes, why occupancy and not
  throughput is the verdict on a multi-XCC card, and the command that regenerates the whole thing. This is
  the file AMD-CASE 4 asserts against and the fixture the Go derivation is unit-tested against.
- `references/amd-hip-symbol-manifest.md` — AMD: the measured exported symbol surface of `libamdhip64`
  **inside the build image**, each interposed name with its version tag and its substitution policy (the
  `hipGetDeviceProperties` → `…R0600` rewrite in particular), the image digest it came from, and the command
  that regenerates it.
- `csrc/thead/ppu-slicing-shim/README.md` — THead: how to build the shims and the reader
  (`build.sh lib|tool|test|unit|check v1`) and how to run one by hand, plus the whole environment contract. The
  build recipes live there, not here.
- `references/thead-usage-region.md` — THead: the usage region as a **contract** — magic, layout version, every
  offset, the frozen lock arena, and the five things a reader must do (refuse an unknown version, take no lock).
  What `tools/ppu-monitor` reads, what case 1 writes with `dd`, and what a metrics scraper should be written
  against instead of a C struct.
- `references/ascend-npu-info-config.md` — Ascend: the 6 config fields, VDie-ID→shm-id, allocator mapping.
- `references/ascend-ld-preload-and-libdcmi.md` — Ascend activation via `/etc/ld.so.preload`; **why libdcmi must
  be preloaded** (weak dcmi syms); the `libc_sec`/CANN-image requirement.
- `references/ascend-npu-smi-and-aicore.md` — Ascend: why the `rt*` layer alone cannot show the slice in
  `npu-smi` (the three layers `rt*` / `dcmi` / `dsmi`, and the measured `npu-smi` → `libdrvdsmi_host.so` call
  chain), **the shipped `ENPU_DSMI_HOOK` fix** and which fields it deliberately leaves card-wide, the
  in-container id-numbering table (the interfaces disagree); AICore-quota mechanism, the benign CANN-8.5.0
  warnings, the unverified-throttle gap.
- `references/nvidia-hami-core-vgpu.md` — NVIDIA: what `libvgpu.so` is, the env+mount injection contract, the
  one-CUDA-major-per-container rule, HAMi-core knobs.
- `references/nvidia-smi-and-sm-limit.md` — NVIDIA: memory limit is directly visible in `nvidia-smi` (NVML
  hook); the SM/compute limit is a time-slice throttle (HAMi log / under-load only); CUDA-13 probe gotchas.
- `references/thead-hgml-dlsym-and-ppu-smi.md` — THead: why `ppu-smi`'s `dlopen` + explicit-handle `dlsym`
  makes a symbol-defining preload inert, the measured `dladdr` table, why the negative control must prove its
  own load, and the two memory getters' structs.
- `references/thead-ppu-sdk-and-glibc.md` — THead: the ROCm-style deployment model, the SDK layout and its
  three `bin` directories, `hgml.h`'s zero includes, the `GLIBC_2.17` assertion, the version axes that do not
  map onto each other.
- `references/thead-hggc-symbol-manifest.md` — THead: the measured exported symbol surface of `libhggc.so`,
  `libhgml.so` and `libhggcrt`, the image digest it came from, and the command that regenerates it.
- `references/troubleshooting.md` — all three backends: scp banner, buildx-missing, Ascend
  link/segfault/hgemm, NVIDIA runtime/preload/stale-cache/cuCtxCreate-v4/stub-lib/SM-visibility, THead
  `ppu-smi` exit-0 / `nm -D` false pass / plain-vs-`_v2` ABI / `nerdctl` namespace and mirrors.
