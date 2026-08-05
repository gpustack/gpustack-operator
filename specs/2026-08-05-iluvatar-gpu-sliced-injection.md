# Spec: Iluvatar GPU Logical Slicing — HAMi-core Injection

Status: Shipped
Type: Feature

## Summary
Iluvatar is one of two accelerator vendors (with AMD) that advertises **no slicing capability** and
registers only whole-card allocation modes — a sliced Iluvatar pool cannot exist today. This spec makes
Iluvatar a first-class logically-sliced vendor by mirroring the shipped NVIDIA / T-Head (#83) slicing
implementation key-for-key: the detector advertises `AcceleratorLogicalSliced` per card, the allocator
registers a Sliced device-plugin server that injects HAMi-core `libvgpu.so` through `/etc/ld.so.preload`,
and the image stages that library into an `iluvatar/` lib dir. The enforcement premise is verified at the
symbol level, not assumed: against a real `corex-driver-4.5.0` `libcuda.so.1`, HAMi-core's entire
memory/compute quota path (`cuMemAlloc_v2`, `cuMemFree_v2`, `cuMemGetInfo_v2`, `cuLaunchKernel`, `cuInit`,
`cuGetProcAddress_v2`, …) is present; of 213 required CUDA symbols only 10 non-quota ones are truly missing
(the rest recovered by HAMi-core's built-in `_v2→_v1` version downgrade), and of 245 NVML symbols only 4
display-only ones are absent. Because Iluvatar's corex is a CUDA-compatible layer that already exposes
`libcuda.so.1` and installs `libixml.so` as `libnvidia-ml.so`, the operator's existing HAMi-core
`libvgpu.so` (built once for NVIDIA CUDA 12) serves Iluvatar with **no new Dockerfile build stage**.

## Motivation
### Goals
- **A sliced Iluvatar container gets real per-slice isolation** — a hard per-card VRAM cap and an SM budget
  from `.sliced.cores-percentage` / `.sliced.memory-percentage` / `.sliced.memory-mib`, enforced
  in-container by HAMi-core, exactly as NVIDIA / T-Head slices already are.
- **The scheduling chain materializes for Iluvatar automatically** (ResourceFlavor → ClusterQueue →
  InstanceType), with no separate gpu-manager + volcano stack.
- **Mirror the shipped NVIDIA / T-Head template exactly** — detector sets `LogicalSliced` in the vendor's
  per-accelerator status and calls `device.SetGroupSlicedDetails`; allocator gains a `!opts.NoSliced`-gated
  Sliced server and a `GetContainerAllocateResponse` branch reusing the public `deviceplugin.SlicedCoresPercent`
  / `SlicedMemoryMib` helpers. No changes to `pkg/nodefeature`, the Pod webhook, Kueue credits, or the
  `.sliced` sizing in `pkg/deviceplugin`.
- **A logical slice is exactly one card.** The Pod webhook already caps a logical-slice request at a single
  card (`pkg/worker/webhooks/worker/pod.go:669`, "a logical slice request is always a single card;
  multi-card logical slicing is not supported yet"), so the allocator adds no card-count guard of its own and
  the injection emits a single `CUDA_DEVICE_MEMORY_LIMIT_0` alongside one un-indexed `CUDA_DEVICE_SM_LIMIT` —
  identical to the NVIDIA branch's single-card output.
- **Injection failure is diagnosable** (Story 3): a sliced Iluvatar Pod carries `runtimeClassName: iluvatar`
  so `ix-container-runtime` injects corex; the allocator stages `libvgpu.so` + `ld.so.preload` and fails the
  allocation with an actionable message if it cannot, rather than letting the container crash on an opaque
  driver error.
- **Success criteria (code-only, no hardware):**
  1. `cores% = 25`, `memory-percentage = 50` on a 32 GiB card → `CUDA_DEVICE_SM_LIMIT = 25`,
     `CUDA_DEVICE_MEMORY_LIMIT_0 = 16384m`; absent `cores%` → 100.
  2. A sliced request is single-card; the branch emits exactly one `CUDA_DEVICE_MEMORY_LIMIT_0` and one
     un-indexed `CUDA_DEVICE_SM_LIMIT`.
  3. A workload declaring its own `LIBCUDA_LOG_LEVEL` keeps it.
  4. Detector sets `LogicalSliced`; `SetGroupSlicedDetails` folds it in; `.sliced`/`.sliced.units`/
     `.sliced.cores-percentage` are advertised.
  5. `${GPUSTACK_LIB_DIR}/iluvatar/libvgpu.so` and `.../iluvatar/ld.so.preload` land in the built image;
     `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./...`, `make lint`, and the new table tests pass on
     darwin.

### Non-Goals
- **Reproducing the vcuda contract** (`vcuda.sock` gRPC + CRI PID lookup + `resource_data_t`/`pids.config`
  writers + vdriver mounts). Needs Iluvatar's private corex-adapted `libcuda-control.so` and would bypass
  `ix-container-runtime`. In Alternatives.
- **Augmenting `binding/ixml` from the official corex header, and gating the capability on a corex runtime
  mode (host-vGPU / driver version).** Investigated and deliberately dropped: every logically-sliced vendor
  in this codebase (NVIDIA, T-Head, MThreads, …) advertises the capability unconditionally in its
  detector's status block, with no runtime-mode gate; a host-vGPU (SR-IOV) gate would be a novel pattern no
  sibling has, guarding a virtual-machine state that does not occur on a bare-metal container node, using
  logic no available hardware can verify. The current mixed `binding/ixml` header (NVIDIA `nvml.h` plus 12
  hand-added `ixml*` for corex-proprietary APIs) already backs the detector correctly. Deferred to a
  dedicated binding spec if a real consumer ever appears; noted in Alternatives.
- **Multi-card logical slicing.** Blocked upstream by the Pod webhook (single-card, deferred for all
  vendors); this spec mirrors that and changes nothing under the webhook.
- **corex VM-level vGPU (SR-IOV).** `libixml.so` carries a full `sysfs_vgpu.cpp` / `ixmlDeviceGet*Vgpu*`
  surface and the driver builds with `--with-vgpu` by default, but that is virtual-machine partitioning, not
  container soft-slicing; neither detected nor driven.
- **corex MIG-style GpuInstance/ComputeInstance API.** The official header declares
  `ixmlDeviceCreateGpuInstance` etc., but a declaration is not hardware support (NVIDIA's header declares MIG
  for all cards too). No `PhysicalSliced` is claimed for Iluvatar.
- **Changing the `.sliced` sizing/credit math, the webhook gates, or AMD.**
- **Real-hardware validation.** No Iluvatar GPU is available; enforcement is symbol-level verified, and
  runtime-semantic confirmation is deferred (Open Questions).

## Proposal
Make Iluvatar a logically-sliced vendor end to end — advertise a `LogicalSliced` capability, register a
Sliced allocator server that injects HAMi-core at Allocate, and stage the library into the image — so an
Iluvatar sliced pool works (advertise → schedule → allocate → inject) once hardware-confirmed, while the
parts that don't need hardware are fully verified now. Every piece mirrors the shipped NVIDIA / T-Head (#83)
implementation.

### User Stories
#### Story 1
As a platform user on an Iluvatar cluster, I want a request for a quarter of a card to come with a real VRAM
ceiling and compute budget, so that co-tenant models on the same card cannot exhaust each other's memory.
#### Story 2
As a cluster administrator, I want Iluvatar sliced pools to appear in the standard scheduling chain like
NVIDIA and T-Head ones, so that I don't run a separate gpu-manager + volcano stack for one vendor.
#### Story 3
As an operator, when a node lacks the corex runtime the injection depends on, I want the allocation refused
with an actionable message (and the `runtimeClassName` requirement documented), so that I don't debug an
opaque in-container driver failure.

### Core Features & Acceptance Criteria
| # | Feature | Acceptance |
| --- | --- | --- |
| F1 | Detector capability | `pkg/devicemanager/detector/iluvatar/device.go` sets `LogicalSliced{Count: 100, CoresPercentageOvercommit: true}` in the per-accelerator status and calls `device.SetGroupSlicedDetails(grpList)`. No new detector test — the flat-constant branch mirrors the NVIDIA soft-slice branch and MThreads, both of which ship it testless; the aggregation it feeds is already covered by `pkg/device/sliced_test.go` |
| F2 | Sliced server registration | `New()` appends a `!opts.NoSliced`-gated `DeviceAllocationModeSliced` server; whole-card modes byte-unchanged |
| F3 | Injection branch | `GetContainerAllocateResponse` branches on Sliced → single-card envs (`IX_VISIBLE_DEVICES`, `CUDA_DEVICE_SM_LIMIT`, `CUDA_DEVICE_MEMORY_LIMIT_0`, `CUDA_DEVICE_MEMORY_SHARED_CACHE`, quiet `LIBCUDA_LOG_LEVEL=0` default) + mounts (`/etc/ld.so.preload`, `/usr/local/vgpu/libvgpu.so`, vgpu lock/cache, `/dev/shm`). Single library, no cuda-major fan-out |
| F4 | Image assets | `${GPUSTACK_LIB_DIR}/iluvatar/libvgpu.so` (`COPY --from=xbuild-nvidia-cuda-12`, no new build stage) + `rootfs/etc/gpustack/lib/iluvatar/ld.so.preload` = `/usr/local/vgpu/libvgpu.so` |
| F5 | Docs | Iluvatar joins the per-vendor slicing matrix's "preload library + per-container compute/VRAM quota" row, noting the corex-compat basis, the `runtimeClassName: iluvatar` requirement, and the hardware-unvalidated status |

### Notes / Constraints / Caveats
- **HAMi-core ↔ corex 4.5.0 is symbol-verified, not assumed** (raw evidence in the Appendix). Quota
  enforcement lives in the CUDA driver layer (`HAMi-core/src/allocator/allocator.c` `oom_check` on
  `cuMemAlloc_v2`); NVML hooking is `#ifdef HOOK_NVML_ENABLE` — display only, never isolation.
- **Why cuda-12, precisely.** HAMi-core hooks `cuCtxCreate_v4` (CUDA 12.5+) / `cuGetProcAddress_v2` (12+)
  that a CUDA-10.2 header cannot declare, so it is the *minimum compilable target*; it shares corex's single
  `#if CUDA_VERSION < 13000` code path and keeps `_v2/_v3` downgrade starting points. **cuda-13 is wrong** —
  its `>= 13000` branch compiles out `cuCtxCreate_v2/_v3`, removing the downgrade anchors corex's 10.2-level
  driver needs. corex is a CUDA-10.2-compatible layer (`libcudart.so.10.2`; toolkit filename
  `..._10.2.run`).
- **corex needs only headers, not linkage** (`HAMi-core/CMakeLists.txt:13` adds `${CUDA_HOME}/include`
  only) — so reusing the `xbuild-nvidia-cuda-12` artifact for the `iluvatar/` lib dir is sound; no new build
  stage, no Iluvatar private registry.
- **HAMi-core's version-downgrade rescue** (`HAMi-core/src/cuda/hook.c:238` `prior_function`) walks a
  missing `cuFoo_v4 → _v3 → _v2 → cuFoo` per symbol at load time, and returns NULL for an app `dlsym` it
  cannot resolve (`libvgpu.c:145`); the built-in fallback library path is `/usr/local/vgpu/libvgpu.so`
  (`libvgpu.c:99`), matching the container mount point reused from the NVIDIA branch.
- **A logical slice is single-card** (`pkg/worker/webhooks/worker/pod.go:669`), so the NVIDIA branch's
  `for i := range accels` loop runs exactly once here — a single `CUDA_DEVICE_MEMORY_LIMIT_0` — and the
  allocator needs no card-count guard.
- **Sliced Iluvatar Pods require `runtimeClassName: iluvatar`** so `ix-container-runtime` injects corex;
  without it the preloaded libvgpu has nothing to hook.
- **`Count: 100` is grounded in Iluvatar's own model** — `vcuda-core` is 1%-granular with 100 == a whole
  card — unlike NVIDIA's 128 (a CUDA-process ceiling). It is one detector constant with no downstream
  coupling (Open Question 3).

### Boundaries
- **Always:** follow the shipped NVIDIA / T-Head branch shape and the shared `Sliced*` helpers; keep
  whole-card modes byte-identical; cover every new behavior with darwin-runnable table tests.
- **Ask first:** before adding any new Dockerfile build stage, any cgo/`_linux.go` seam, any dependency on
  an Iluvatar private registry, or any change to `binding/ixml`.
- **Never:** ship a vcuda `libcuda-control.so` dependency; claim `PhysicalSliced` for Iluvatar; alter
  `.sliced` sizing/credit math; drive corex VM-level vGPU or GpuInstance APIs; state hardware-verified
  behavior no hardware verified.

### Risks and Mitigations
- corex lacks a symbol HAMi-core needs on a hot path → **already refuted at symbol level** (Appendix);
  residual risk is runtime semantics only, isolated to one injection function, with the vcuda path as the
  documented fallback.
- `Count: 100` proves optimistic for corex context limits → one detector constant, no downstream coupling.
- Docs claim unconfirmed capability → F5 states the unvalidated status explicitly, as the MThreads/Hygon and
  T-Head specs did.

## Design Details
### Commands
Environment (confirmed with the user): Go build/test/lint run **locally on darwin** (the whole module,
CGO vendor bindings and detectors included, compiles and unit-tests on darwin); the image smoke build (T1)
runs on the **local docker** host.
```bash
# Go (local darwin)
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./...
go test ./pkg/devicemanager/allocator/iluvatar/... ./pkg/devicemanager/detector/iluvatar/...
make lint          # whole-module; allow a long timeout on a cold cache

# Image smoke (local docker) — mask non-nvidia/iluvatar xbuild COPYs for a fast build
docker buildx build --target gpustack -f pack/gpustack-operator/Dockerfile .
```
### Project Structure
```
pkg/devicemanager/detector/iluvatar/device.go        # F1 capability
pkg/devicemanager/detector/iluvatar/device_test.go   # F1 table test
pkg/devicemanager/allocator/iluvatar/deviceplugin.go # F2 server + F3 branch
pkg/devicemanager/allocator/iluvatar/deviceplugin_test.go # F3 table tests
pack/gpustack-operator/Dockerfile                    # F4 COPY + install
pack/gpustack-operator/rootfs/etc/gpustack/lib/iluvatar/ld.so.preload # F4
docs/architecture/discovery.md, README.md            # F5
```
### Code Style
```go
// F3: a logical slice is single-card and corex presents a single CUDA-compatible
// driver level, so no per-card index fan-out and no CUDA-major reconciliation.
libDir := filepath.Join(deviceplugin.OperatorLibDir, Manufacturer)
mounts := []*deviceplugin.Mount{
    {ContainerPath: ctrLdPreloadPath, HostPath: filepath.Join(libDir, "ld.so.preload"), ReadOnly: true},
    {ContainerPath: ctrVgpuLibPath, HostPath: filepath.Join(libDir, "libvgpu.so"), ReadOnly: true},
}
```
### Implementation Plan
- [x] **T1 · Image asset: stage libvgpu.so for Iluvatar**
      Blocked by: None
      Owns: `pack/gpustack-operator/Dockerfile`, `pack/gpustack-operator/rootfs/etc/gpustack/lib/iluvatar/**`
      Gate: review
      Acceptance: `${GPUSTACK_LIB_DIR}/iluvatar/libvgpu.so` populated by `COPY --from=xbuild-nvidia-cuda-12 /out`
        (no new build stage); `${GPUSTACK_LIB_DIR}/iluvatar/ld.so.preload` = `/usr/local/vgpu/libvgpu.so`
        installed from rootfs, mirroring the nvidia/thead pair (`Dockerfile:415`, `:507`).
      Verify: `docker buildx build --target gpustack -f pack/gpustack-operator/Dockerfile .` reaches the
        COPY/install and the final image carries both files (fast local build, non-nvidia/iluvatar xbuild
        COPYs masked per the nvidia-only recipe).

- [x] **T2 · Detector: advertise Iluvatar logical slicing**
      Blocked by: None
      Owns: `pkg/devicemanager/detector/iluvatar/**`
      Acceptance: the per-accelerator status sets `LogicalSliced{Count:100, CoresPercentageOvercommit:true}`
        and the detector calls `device.SetGroupSlicedDetails(grpList)` before return (mthreads/nvidia shape).
        No new detector test: the flat-constant branch mirrors the NVIDIA soft-slice branch and MThreads,
        both testless; the aggregation is covered by `pkg/device/sliced_test.go`.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./pkg/devicemanager/detector/iluvatar/...`
        and `go test ./pkg/devicemanager/detector/iluvatar/...` (compiles; reports no test files).

- [x] **T3 · Allocator: HAMi-core sliced injection branch**
      Blocked by: None
      Owns: `pkg/devicemanager/allocator/iluvatar/**`
      Gate: review
      Acceptance: `New()` registers a `!opts.NoSliced`-gated Sliced server; `GetContainerAllocateResponse`
        branches on Sliced → single-card injection: envs `IX_VISIBLE_DEVICES`, `CUDA_DEVICE_SM_LIMIT`,
        `CUDA_DEVICE_MEMORY_LIMIT_0`, `CUDA_DEVICE_MEMORY_SHARED_CACHE`, quiet `LIBCUDA_LOG_LEVEL=0`; mounts
        `/etc/ld.so.preload` + `/usr/local/vgpu/libvgpu.so` from `${OperatorLibDir}/iluvatar`, vgpu
        lock/cache, `/dev/shm`; whole-card modes byte-unchanged. Table tests pin: cores%=25/mem%=50 on
        32GiB → SM_LIMIT=25, MEMORY_LIMIT_0=16384m; absent cores% → 100; self-declared LIBCUDA_LOG_LEVEL kept.
      Verify: `go test ./pkg/devicemanager/allocator/iluvatar/...`

- [x] **T4 · Docs: flip Iluvatar to logical-sliced in the matrix**
      Blocked by: T3
      Owns: `docs/architecture/discovery.md`, `README.md`
      Acceptance: Iluvatar joins the "preload library + per-container compute/VRAM quota" row
        (`discovery.md:248`); the env/mount list mentions `IX_VISIBLE_DEVICES` + libvgpu; the
        `runtimeClassName: iluvatar` requirement + the hardware-unvalidated status are noted; the README
        accelerator matrix row is updated; docs index/links stay consistent.
      Verify: `grep -n Iluvatar docs/architecture/discovery.md` shows the sliced row.

Checkpoints: T1/T2/T3 each leave the tree green independently; T4 lands docs last. T1, T2, T3 are unblocked
with disjoint `Owns:` → `/my-build … team` builds them concurrently.

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
None — no shared test helper or fixture changes. The Iluvatar allocator/detector table tests are new files
alongside the existing ones, and the whole-card allocator cases stay valid unchanged.

#### Unit tests
- `pkg/devicemanager/detector/iluvatar`: `2026-08-05` - None. The detector sets a flat `LogicalSliced`
  constant with no version/family decision to exercise, exactly as the NVIDIA soft-slice branch and MThreads
  do (both ship testless); adding one would only re-cover `device.SetGroupSlicedDetails`, already tested in
  `pkg/device/sliced_test.go`. Ascend has a detector test only because it gates slicing on a runtime helper.
- `pkg/devicemanager/allocator/iluvatar`: `2026-08-05` - new `deviceplugin_test.go` sliced-branch cases: the
  env/mount contract (F3 success criteria), single-card output, `cores%`-absent default, self-declared
  `LIBCUDA_LOG_LEVEL` preserved, whole-card modes byte-unchanged. Target: parity with the NVIDIA allocator
  sliced test coverage.

#### Integration tests
None — there is no integration harness between the detector and allocator; the scheduling-chain wiring
(`.sliced` advertisement → Kueue credits → InstanceType) is owned by the capability spec and covered by the
existing worker-controller tests, which are unchanged. Concrete test names, if any surface, added after the
implementation PR merges.

#### e2e tests
Deferred with justification: no Iluvatar GPU is available. The image-stage build is verified by T1
(`docker buildx`). A live scheduling-chain + in-container isolation run (`ixsmi` shows the capped VRAM, an
over-request is refused, two slices on one card do not cross memory) is a stated Non-Goal and an Open
Question, to run when hardware is available via `gpustack-operator-e2e` /
`gpustack-operator-xbuild-and-verify`. Symbol-level compatibility (Appendix) stands in for the hardware run
until then.

## Alternatives
- **Reproduce the vcuda contract** — every byte attested by vendor source
  (`iluvatar-vgpu-manager/pkg/services/virtual-manager/manager.go`), but needs the private corex
  `libcuda-control.so`, bypasses `ix-container-runtime`, and adds a per-Pod gRPC (`vcuda.sock`) + CRI
  subsystem. Kept as the fallback if HAMi-core proves runtime-incompatible.
- **Augment `binding/ixml` from the official corex header + gate the capability on a corex runtime mode**
  (host-vGPU / driver version) — considered and dropped for this spec: no sibling vendor gates its logical
  capability, the gated state (SR-IOV host-vGPU) does not occur on a bare-metal container node, the logic is
  hardware-unverifiable, and the current mixed header already backs the detector correctly. A full resync to
  the 460-symbol official header would also break the `nvml*` aliases three detector APIs depend on
  (`SystemGetCudaDriverVersion`, `DeviceGetCudaComputeCapability`, `DeviceGetMemoryInfo_v2` are nvml-only in
  the .so). Deferred to a dedicated binding spec if a real consumer appears.
- **Write a corex slicing shim (T-Head `csrc/` approach)** — corex exposes no container-level partition API
  to build on; this amounts to rewriting HAMi-core.
- **Capability-declaration-only** — advertise `.sliced` with no injection; rejected, since sliced containers
  would silently get whole-card access.

## Open Questions
1. Does the cuda-12-built `libvgpu.so` hook corex's CUDA-10.2-level `libcuda.so.1` correctly at **runtime**
   (symbols proven present; semantics unproven)? Blocks GA; first target of hardware verification.
2. Is `ixsmi` inside a slice expected to report the capped VRAM, given corex hooks NVML only when
   `HOOK_NVML_ENABLE`? Confirms whether the 4 missing NVML symbols matter for UX (not for isolation).
3. Is 100 the right per-card slice ceiling, or does corex cap concurrent contexts lower?

## Appendix: corex 4.5.0 Symbol-Level Verification
Raw evidence behind the "HAMi-core enforces on corex" premise, recorded here so the spec is self-contained
and reproducible. Artifacts obtained by unpacking the public Iluvatar SDK on a generic x86_64 Linux host
(no Iluvatar GPU required):

- `corex-driver-linux64-4.5.0_x86_64.run` → `corex/lib64/libcuda.so.1` (17.5 MB), `corex/lib64/libixml.so`
  (514 KB), `corex/bin/ixsmi`. Reported `.corex_version` = `4.5.0`.
- `corex-toolkit-linux64-4.5.0_x86_64_10.2.run` → official header at
  `corex-toolkit/ixdriver/include/IX/ixml/ixml.h` (Iluvatar CoreX © 2026; 460 `ixml*`, 0 `nvml*` symbols).
- Download source (public OSS):
  `https://file-server-software-infra-sh.oss-cn-shanghai.aliyuncs.com/4.5.0/x86_64/sdk/`.

CUDA driver surface (HAMi-core need vs `libcuda.so.1` export):

- HAMi-core requires **213** `cu*` symbols; **26** initially absent; **15** recovered by `prior_function`
  version downgrade; **10** truly missing, none on the quota path (`cuDeviceGetLuid` [Windows-only],
  `cuDeviceGetTexture1DLinearMaxWidth`, `cuFlushGPUDirectRDMAWrites`, and 7 `cuGraph*ExternalSemaphores*`).
- Quota-path spot check — all **PRESENT**: `cuInit`, `cuMemAlloc_v2`, `cuMemAllocManaged`, `cuMemFree_v2`,
  `cuMemGetInfo_v2`, `cuMemAllocAsync`, `cuMemCreate`, `cuArrayCreate_v2`, `cuLaunchKernel`,
  `cuDeviceTotalMem_v2`, `cuGetProcAddress`, `cuGetProcAddress_v2`, `cuCtxCreate_v2`. (`cuCtxCreate_v3/_v4`
  absent but downgrade to `_v2`.)

NVML surface (`libixml.so`):

- Exports **281** `nvml*` and **347** `ixml*` symbols. HAMi-core requires **245** `nvml*`; only **4** absent
  — `nvmlDeviceGetMemoryInfo_v2` (v1 present, ixsmi uses v1), `nvmlDeviceGetProcessesUtilizationInfo`, and
  the NVIDIA-internal `nvmlInternalGetExportTable` / `nvmlRetry_NvRmControl`. All display-only; none affect
  the CUDA-driver-layer isolation.

Detector data-source note: in `libixml.so`, `SystemGetCudaDriverVersion` and `DeviceGetCudaComputeCapability`
export **only** as `nvml*` (no `ixml*` form; the official header does not declare them), which is why the
current mixed `binding/ixml` header keeps the NVIDIA `nvml.h` surface rather than switching wholesale to the
official corex header.
