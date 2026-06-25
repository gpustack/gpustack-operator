# Spec: Accelerator Soft-Slicing Runtime Isolation (NVIDIA HAMi-core / Ascend vcann-rt)

Status: Building

## Summary

The shipped `accelerator-resource-modes-refactor` spec landed the full Sliced **scheduling and accounting**
chain (global denominator D=12800, Webhook unit conversion, Kueue `multiplyBy`, the dual key
`.sliced.units`/`.sliced`, borrow+reclaim), but it explicitly left **runtime isolation** as a Non-Goal: the
device-plugin `Allocate()` only does placement bookkeeping into `Devices.Status` and injects no real
VRAM/compute limits. This spec implements that deferred Non-Goal for the **soft-slicing** path only (hard
slicing — MIG / Ascend vNPU dynamic virtualization — stays out of scope). It vendors two preload libraries as
git submodules and compiles them in the operator image (NVIDIA HAMi-core → `libvgpu.so`; Ascend vcann-rt →
`libvruntime.so` + `enpu-monitor`), stages them onto the host via the device-manager DaemonSet, and rewrites
`GetContainerAllocateResponse` for the NVIDIA and Ascend allocators so a sliced container is launched with the
preload library injected through `/etc/ld.so.preload` and per-container VRAM/compute quota derived from its
`.sliced.units` request. It also adds per-pod working-directory garbage collection so the host does not
accumulate stale slice config/cache directories.

## Motivation

### Goals

1. **Real soft isolation for sliced containers.** When the allocator runs with the soft policy, a container
   that requests a slice (`.sliced.units` / `.sliced`) starts with a preload library injected and per-container
   memory/compute quota applied, instead of seeing the whole physical card. Target users: cluster operators
   running multi-tenant inference/training that over-commit a single GPU/NPU through GPUStack's Sliced mode.
2. **Vendored, version-pinned preload libraries built in-image.** HAMi-core and vcann-rt are added as git
   submodules and compiled via multi-stage Docker builds against pinned vendor SDK base images, producing the
   per-runtime-version `.so` artifacts laid out under `${GPUSTACK_LIB_DIR}` in the final image.
3. **Host staging + container injection.** The device-manager DaemonSet copies the in-image library tree onto
   the host (idempotent, checksum-aware) via an init container; `Allocate()` then mounts the right library +
   `ld.so.preload` + shared-cache/lock directories into the container and writes the per-container quota
   (env for NVIDIA, `npu_info.config` for Ascend).
4. **Quota derived from the request, not hard-coded.** The per-card ratio `R` is computed from the
   container's `.sliced.units` value (`R = units / D`); memory and compute limits are derived from `R` and the
   card's reported memory. The number of per-card memory-limit entries follows the `.sliced` card count.
5. **Deterministic, level-based Ascend vNPU id assignment.** `virtual-npu-id` starts at 0 and is unique among
   vNPUs sharing a physical NPU, assigned by scanning live allocations and taking the lowest free index so the
   result survives restarts and re-reconciles.
6. **Per-pod working-directory garbage collection.** `/var/lib/gpustack/operator/pods/<podUUID>/c-<container>`
   directories are reclaimed once their pod is gone, via a notifier fed pod UUIDs from the reconciler
   (startup scan + mark-and-sweep after 3 consecutive misses).

**Testable success criteria.**
- `copy-dir.sh <src> <dst>` creates a missing destination, recurses, and overwrites a file only when its
  checksum differs — verified by a `docker run --rm` script test.
- The multi-stage image build produces `libvgpu.so` (cuda-12 / cuda-13) and `libvruntime.so` + `enpu-monitor`
  (each CANN/family target) in the right `${GPUSTACK_LIB_DIR}` locations in the final image.
- `GetContainerAllocateResponse` for a sliced NVIDIA container returns the documented env set
  (`CUDA_DEVICE_SM_LIMIT`, `CUDA_DEVICE_MEMORY_LIMIT_<i>`, `CUDA_DEVICE_MEMORY_SHARED_CACHE`) and mounts
  (`libvgpu.so`, `/etc/ld.so.preload`, `/tmp/vgpulock`, `/tmp/vgpu`, `/dev/shm`) — asserted by table-driven
  unit tests against a fixture `Devices` + pod/container.
- `GetContainerAllocateResponse` for a sliced Ascend container renders the documented `npu_info.config`
  (`physical-npu-id`, `virtual-npu-id`, `aicore-quota`, `memory-quota`, `shm-id`, `scheduling-policy=2`) and
  the documented mounts — asserted by table-driven unit tests.

### Non-Goals

- **Hard slicing.** NVIDIA MIG real-instance creation/`nvidia-mig-parted`, Ascend vNPU dynamic virtualization
  (`dcmi_create_vdevice`), MIG/vNPU UUID injection, and the binding(CGo) lifecycle wrappers for create/destroy
  — all out of scope here (covered by a separate hard-slicing follow-up).
- **detector `MaxPartitions` / `slicing-policy` plumbing rework** (report §6.3) — the detector-side
  `MaxPartitions` decision helper, `SoftPartition` real probing, and Ascend partition-feature filling are not
  part of this spec; this spec consumes whatever the existing Sliced chain already advertises.
- **`virtual` partition semantics** and the `qos` policy's physical/virtual branches — soft only.
- **Raw-Pod `.sliced.units` quota-vs-physical alignment** beyond the existing `PadSlicedUnits` safety net
  (prior spec Open Question #5 stays open).
- **MPS compatibility** and **multi-version drift policy** beyond the cuda-12/13 and CANN-8/9 targets named
  here.
- **NVIDIA on-hardware E2E** — there is no NVIDIA machine (no NVIDIA RuntimeClass) in this environment, so
  NVIDIA device-manager E2E runs externally; only build/unit tests run here.

## Proposal

Pick up the soft-slicing branch of the Sliced `Allocate()` path. The device-manager image gains a compiled,
version-keyed library tree; the DaemonSet stages that tree onto the host; and each vendor allocator's
`GetContainerAllocateResponse` turns a bookkeeping-only response into a real injection response (preload
library + quota) whose isolation parameters are computed from the container's already-validated
`.sliced.units` / `.sliced` request. A small reconciler-fed notifier keeps the per-pod working directories
from leaking.

### User Stories

#### Story 1 — Operator runs sliced workloads with real isolation
As a **cluster operator**, once I have enabled slicing on a card model and a user submits a sliced workload, I
want each sliced container to actually see only its fraction of the card's memory and compute, so that one
tenant cannot starve or OOM another sharing the same physical GPU/NPU.

- **NVIDIA:** the container starts with `libvgpu.so` preloaded via `/etc/ld.so.preload`, `CUDA_DEVICE_SM_LIMIT`
  and `CUDA_DEVICE_MEMORY_LIMIT_<i>` set from its request, and a private `/tmp/vgpu` cache.
- **Ascend:** the container starts with `libvruntime.so` preloaded, and `/etc/enpu/vcann-rt/npu_info.config`
  carrying its `aicore-quota` / `memory-quota` and a unique `virtual-npu-id`.

#### Story 2 — Maintainer builds and ships the operator image with preload libraries
As a **GPUStack maintainer**, I want the preload libraries vendored as submodules and compiled in the operator
image against pinned vendor SDK bases, so that the final image carries `libvgpu.so` (per CUDA major) and
`libvruntime.so` + `enpu-monitor` (per CANN/family) at deterministic paths, and a local multi-stage build
verifies each artifact is produced and placed.

#### Story 3 — Operator's host stays clean across pod churn
As a **cluster operator**, I want the device-manager to copy the library tree onto the host idempotently and to
reclaim each pod's slice working directory after the pod is gone, so that node disk does not fill with stale
`pods/<uuid>` config/cache directories over time.

### Core Features & Acceptance Criteria

| # | Feature | Acceptance criteria (testable) |
|---|---|---|
| F1 | `copy-dir.sh` host-staging helper | `copy-dir.sh <src> <dst>` recurses; creates `<dst>` if absent; overwrites an existing destination file only when checksums differ. `docker run --rm` script test covers: missing-dir create, identical-file skip, changed-file replace. |
| F2 | NVIDIA multi-stage build (HAMi-core) | `nvidia/cuda:13.0.3-cudnn-devel-ubi8` and `nvidia/cuda:12.9.2-cudnn-devel-ubi8` builder stages each emit `libvgpu.so`; the final image carries `${GPUSTACK_LIB_DIR}/nvidia/cuda-13/libvgpu.so` and `${GPUSTACK_LIB_DIR}/nvidia/cuda-12/libvgpu.so`. Local build inspection. |
| F3 | Ascend multi-stage build (vcann-rt) | The five CANN/family builder stages each emit `libvruntime.so` + `enpu-monitor`; the final image carries them under `${GPUSTACK_LIB_DIR}/ascend/cann-{8,9}-{910b,910c,950}` per the mapping. Local build inspection. |
| F4 | `ld.so.preload` rootfs assets | Final image has `/etc/gpustack/lib/nvidia/ld.so.preload` (`/usr/local/vgpu/libvgpu.so`, mode 0644) and `/etc/gpustack/lib/ascend/ld.so.preload` (`/opt/enpu/vcann-rt/lib/libvruntime.so`, mode 0644). |
| F5 | DaemonSet host staging | The device-manager DaemonSet mounts host `/tmp`; an init container runs `copy-dir.sh /etc/gpustack/lib /var/lib/gpustack/operator/lib`. Chart render test. |
| F6 | NVIDIA sliced `GetContainerAllocateResponse` | For a sliced container, the response carries `CUDA_DEVICE_SM_LIMIT=R`, `CUDA_DEVICE_MEMORY_LIMIT_<i>` (Mi→Ki × R, one per `.sliced` card), `CUDA_DEVICE_MEMORY_SHARED_CACHE=/tmp/vgpu/cudevshr.cache`; mounts `/tmp/vgpulock`(rw), `ld.so.preload`→`/etc/ld.so.preload`(ro), `cuda-{Major|12}/libvgpu.so`→`/usr/local/vgpu/libvgpu.so`(ro), `pods/<X>/tmp/vgpu`→`/tmp/vgpu`(rw), `/dev/shm`→`/dev/shm`. Creates `/tmp/vgpulock`(0777), `pods/<X>`(0777), `pods/<X>/tmp/vgpu`(0777). Table test. |
| F7 | Ascend sliced `GetContainerAllocateResponse` | For a sliced container, renders `pods/<X>/etc/enpu/vcann-rt/npu_info.config`(0644) with `physical-npu-id`=accelerator Index, unique `virtual-npu-id` (lowest-free per physical NPU, from 0), `aicore-quota`=R, `memory-quota`=R × memory, `shm-id`=accelerator Id (spaces→`-`), `scheduling-policy=2` (vcann-rt *elastic* policy, the upstream default); mounts `ld.so.preload`→`/etc/ld.so.preload`(ro), `cann-{Major|8}-{lower(Family)}/{lib/libvruntime.so,tools/enpu-monitor}`→`/opt/enpu/vcann-rt/...`, the config file→container path, `/dev/shm`→`/dev/shm`. Creates `pods/<X>`(0777). Table test. |
| F8 | Quota ratio `R` derivation (round down) | `R` is derived from the container's `.sliced.units` value (`R = units / D`, D=12800); every `R`-derived quota is **floored** (round down): compute percent = `floor(R×100)`, NVIDIA `CUDA_DEVICE_MEMORY_LIMIT_<i>` = `floor(memKi × R)`, Ascend `aicore-quota` = `floor(R×100)`, `memory-quota` = `floor(memMB × R)`. The count of `CUDA_DEVICE_MEMORY_LIMIT_<i>` entries equals the `.sliced` card count. Unit test over the worked examples below. |
| F9 | Per-pod working-dir GC | The notifier channel changes from `chan struct{}` to `chan []string` carrying the **live pod-UUID list** (empty/nil ⇒ no pods on the node). The Sliced `ResourceServer` scans `pods/<uuid>` on startup; a UUID present on disk but absent from the latest list for 3 consecutive notifications is removed; a list UUID absent on disk is tracked. Unit test driving the notifier with successive lists (incl. nil) asserts removal only after 3 misses. |

**`R` worked examples (D=12800, container key `X = <podUUID>/c-<containerName>`):**

| Request | `.sliced.units` (per card) | `.sliced` (cards) | R | NVIDIA `CUDA_DEVICE_SM_LIMIT` | NVIDIA `CUDA_DEVICE_MEMORY_LIMIT_*` (24 GiB card) |
|---|---|---|---|---|---|
| 1/8 card | 1600 | 1 | 1600/12800 = 1/8 | `floor(12.5)` = 12 | `LIMIT_0` = `floor(24Gi→Ki × 1/8)` |
| 1/4 card | 3200 | 1 | 1/4 | `floor(25)` = 25 | `LIMIT_0` |
| 1/8 × 2 cards | 1600 | 2 | 1/8 | 12 | `LIMIT_0`, `LIMIT_1` |

All `R`-derived quotas **round down (floor)**: `CUDA_DEVICE_SM_LIMIT` / `aicore-quota` are integer percents
`floor(R×100)`; memory limits are floored (HAMi-core Ki, vcann-rt MB).

### Notes / Constraints / Caveats

- Go (controller-runtime device plugin) + multi-stage Dockerfile + Helm chart + shell. Follow the
  Go / Kubernetes / testing conventions in `CLAUDE.md`.
- **Activation is `/etc/ld.so.preload`, never the `LD_PRELOAD` env** — it covers child processes and is harder
  for the workload to clobber (report §2.3 / §4.1).
- **Path convention (confirmed):** the host library tree lives at
  `/var/lib/gpustack/operator/lib/{nvidia,ascend}/...` and per-pod working dirs at
  `/var/lib/gpustack/operator/pods/<X>/...`. (The task draft's `/var/lib/gpustack/operator/operator/lib/...`
  double-`operator` segment was a typo, normalized to a single `operator`.)
- **`GPUSTACK_LIB_DIR`** is a new Dockerfile `ARG` = `${GPUSTACK_CONF_DIR}/lib` = `/etc/gpustack/lib`.
- **`RuntimeVersion.Major`** selects the library subdir; when empty, default to `cuda-12` (NVIDIA) /
  `cann-8` (Ascend). `Family` (lower-cased) selects the Ascend SoC subdir.
- **Ascend `shm-id`** is the accelerator Id with spaces replaced by `-` — the sample id
  `E0F4EE64 802061B1 6A691492 89528485 104301E3` becomes the hyphen-joined form the vcann-rt README
  prescribes (its VDie-ID example `11111111-22222222-…`), guaranteeing global uniqueness.
- **GC notifier payload:** the `DevicesReconciler` notifier carries the live pod-UUID list as `chan []string`
  (replacing `chan struct{}`); an empty/nil slice means the node currently has no pods.
- After API/webhook/generated changes (none expected here) run `make generate`; run `make lint` after Go
  changes.
- Submodules add a `.gitmodules` (currently none in repo) and `make deps`/build must clone them.

### Boundaries

- **Always:** activate via `/etc/ld.so.preload`; keep `Allocate()` idempotent and safe to repeat; derive quota
  from the request (`.sliced.units` / `.sliced`), never hard-code; pin submodule versions; run `make lint`
  after Go changes; add table-driven unit tests for every new response/helper.
- **Ask first:** changing the host path layout under `/var/lib/gpustack/operator`; adding hard-slicing code;
  changing D or the `.sliced.units`→R relationship; touching the detector / `slicing-policy` plumbing;
  selecting `vcann-rt` vs `hami-vnpu-core` for Ascend (this spec commits to `vcann-rt` per the task).
- **Never:** inject via `LD_PRELOAD` env instead of `ld.so.preload`; expose the whole physical card to a sliced
  container; copy library files without the checksum guard; delete a pod working dir before the 3-miss
  confirmation; pin submodules to a moving ref (`@latest`/branch tip).

### Risks and Mitigations

1. **Compiler/SDK base drift produces an ABI-mismatched `.so`** → submodules track the latest upstream commit
   (recorded in the gitlink) and base images use the fixed tag list in Build Targets (no digest pinning);
   verify each artifact exists and loads in the build stage.
2. **`copy-dir.sh` overwrites a library a running container is mmap-ing** → checksum-guarded copy only replaces
   on change; staging runs in an init container before workloads on that node would mount the new tree.
3. **Ascend `virtual-npu-id` collision on a shared physical NPU** → assign by scanning live allocations and
   taking the lowest-free index (level-based); a unit test asserts two concurrent slices on one NPU get
   distinct ids.
4. **Per-pod working dir leak / premature deletion** → reconciler-fed notifier with a startup scan and a
   3-consecutive-miss mark-and-sweep before removal.
5. **`RuntimeVersion.Major` empty or unmapped** → documented defaults (cuda-12 / cann-8); a missing library
   path fails the allocate loudly rather than silently exposing the full card.
6. **No NVIDIA hardware here** → NVIDIA correctness is covered by unit tests on the response shape; on-hardware
   E2E is external. Ascend can be exercised on hardware (see Test Plan).
7. **Notifier payload change is cross-cutting (T8)** → widening the `DevicesReconciler` notifier from
   `chan struct{}` to `chan []string` touches every mode/vendor `ListAndWatch` loop, not just sliced. Mitigation:
   non-sliced consumers ignore the payload and keep treating any send as a change signal; a unit test asserts
   the existing `ListAndWatch` still emits a response on every notification for Exclusive/Shared.

## Design Details

### Commands

```bash
# Submodules (added by this spec; tracked at latest upstream commit)
git submodule update --init

# Incremental per-stage build-verify (add one builder stage at a time, --target each)
docker buildx build -f pack/gpustack-operator/Dockerfile --target cannbuild-8-910b -t v .  # then 8-910c, 9-910b, 9-910c, 9-950
docker buildx build -f pack/gpustack-operator/Dockerfile --target nvbuild-12  -t v .        # then nvbuild-13

# Full image + artifact-layout check (F2/F3/F4)
docker buildx build -f pack/gpustack-operator/Dockerfile -t gpustack-operator:soft-slicing .
# inspect: ${GPUSTACK_LIB_DIR}/ascend/cann-{8,9}-{910b,910c,950}/{lib/libvruntime.so,tools/enpu-monitor};
#          ${GPUSTACK_LIB_DIR}/nvidia/cuda-{12,13}/libvgpu.so; both ld.so.preload assets (0644)

# copy-dir.sh behavioral test (F1)
docker run --rm <img> bash -c '/usr/bin/copy-dir.sh /src /dst'   # assert: create / skip-identical / replace-changed

# Go
make lint                                   # required after Go changes
make test ./pkg/deviceplugin/... ./pkg/devicemanager/allocator/ascend/... ./pkg/devicemanager/allocator/nvidia/...

# Chart render (F5)
helm template deploy/gpustack-operator/chart --show-only templates/device-manager/daemonset.yaml
```

### Project Structure (files in scope)

```
.gitmodules                                              # NEW: hami-core + vcann-rt submodules
pack/gpustack-operator/external/nvidia/libvgpu/          # NEW submodule: HAMi-core
pack/gpustack-operator/external/ascend/libvnpu/          # NEW submodule: vcann-rt
pack/gpustack-operator/Dockerfile                        # add GPUSTACK_LIB_DIR ARG; nvbuild/cannbuild stages; copy artifacts + copy-dir.sh + ld.so.preload
pack/gpustack-operator/rootfs/usr/bin/copy-dir.sh        # NEW: recursive, checksum-guarded copy
pack/gpustack-operator/rootfs/etc/gpustack/lib/nvidia/ld.so.preload   # NEW: /usr/local/vgpu/libvgpu.so
pack/gpustack-operator/rootfs/etc/gpustack/lib/ascend/ld.so.preload   # NEW: /opt/enpu/vcann-rt/lib/libvruntime.so
deploy/gpustack-operator/chart/templates/device-manager/daemonset.yaml   # host /tmp mount; init container copy-dir.sh
pkg/devicemanager/allocator/nvidia/deviceplugin.go       # sliced GetContainerAllocateResponse: preload + env + mounts + dirs
pkg/devicemanager/allocator/ascend/deviceplugin.go       # sliced GetContainerAllocateResponse: preload + npu_info.config + mounts + dirs
pkg/deviceplugin/server.go / helper.go                   # R-from-request helper; shared dir/mount builders; notifier-fed pod-dir GC
pkg/deviceplugin/controller.go                           # feed live pod UUIDs into the ResourceServer notifier
```

### Build Targets (base image → artifact → final-image path)

Submodules track the **latest upstream commit** at add time (recorded in `.gitmodules` / the submodule
gitlink); base images are referenced by the **tags below, not pinned digests**.

**NVIDIA — HAMi-core → `libvgpu.so`** (`GPUSTACK_LIB_DIR = /etc/gpustack/lib`):

| Builder base image | Artifact | Final-image path |
|---|---|---|
| `nvidia/cuda:13.0.3-cudnn-devel-ubi8` | `libvgpu.so` | `${GPUSTACK_LIB_DIR}/nvidia/cuda-13/libvgpu.so` |
| `nvidia/cuda:12.9.2-cudnn-devel-ubi8` | `libvgpu.so` | `${GPUSTACK_LIB_DIR}/nvidia/cuda-12/libvgpu.so` |

**Ascend — vcann-rt → `libvruntime.so` + `enpu-monitor`:**

| Builder base image | Final-image dir (`${GPUSTACK_LIB_DIR}/ascend/...`) |
|---|---|
| `quay.io/ascend/cann:8.5.0-910b-ubuntu22.04-py3.11`        | `cann-8-910b` |
| `quay.io/ascend/cann:8.5.0-a3-ubuntu22.04-py3.11`          | `cann-8-910c` |
| `quay.io/ascend/cann:9.1.0-beta.1-910b-ubuntu22.04-py3.12` | `cann-9-910b` |
| `quay.io/ascend/cann:9.1.0-beta.1-a3-ubuntu22.04-py3.12`   | `cann-9-910c` |
| `quay.io/ascend/cann:9.1.0-beta.1-950-ubuntu22.04-py3.12`  | `cann-9-950`  |

Each Ascend dir holds `lib/libvruntime.so` and `tools/enpu-monitor`. The allocator resolves the dir as
`cann-<RuntimeVersion.Major>-<lower(Family)>` (default `cann-8` when `Major` is empty); NVIDIA resolves
`cuda-<RuntimeVersion.Major>` (default `cuda-12` when empty).

> **CANN 9 = 9.1.0-beta.1, not 9.0.0** (build finding): vcann-rt (master) fails to compile against the CANN
> 9.0.0 toolkit — its `acl/acl_dump.h` references an undefined `acldumpType` — but builds cleanly against
> 9.1.0-beta.1 (matching the README's "CANN 8.5 / 9.1" support claim). All five CANN images are multi-arch
> (amd64 + arm64), so buildkit builds each `cannbuild-*` stage for the final image's target arch.
>
> **dcmi link stub:** the CANN toolkit ships no driver `dcmi` (the HDK driver is host-injected at runtime), so
> the build compiles the in-tree `test/stub/dcmi_stub.c` into a stub `libdcmi.so` (SONAME `libdcmi.so`) to
> satisfy `-ldcmi`; the dcmi entry points are weak, `--as-needed` drops the unused stub from `NEEDED`, and the
> real driver libdcmi binds at runtime. `enpu-monitor` (an executable) additionally pulls in, transitively
> through the vendor `.so` files, driver/toolkit symbols that a toolkit-only image (no host HDK driver) cannot
> resolve at link time — the HAL entry points (`drv*`/`hal*`) in `libascend_hal.so` and `ErrorManager::*` in
> `liberror_manager.so`; these bind at runtime where the real driver and full toolkit are present. The build
> links with `-Wl,--allow-shlib-undefined` (via `LDFLAGS`, which CMake seeds into `CMAKE_EXE_LINKER_FLAGS`) so
> the executable links against the SDK as shipped while its own direct deps (dcmi stub, `c_sec`, `ascendcl`)
> stay strictly resolved. This is **arch-agnostic** (fixes amd64 + arm64). An earlier `uname -m`-keyed
> `libascend_hal.so` LD_LIBRARY_PATH probe was rejected: the toolkit's `<arch>-linux` tree carries both the host
> HAL and the device-side (always AArch64) HAL, so the path-name match non-deterministically picked the
> wrong-arch stub on amd64 (ld rejected it as `libascend_hal.so ... not found`), and even when the HAL resolved,
> the `liberror_manager.so` reference still failed — `--allow-shlib-undefined` covers the whole transitive
> closure in one stroke.

### Code Style

```go
// sliceRatio derives the per-card fraction R from the container's ".sliced.units"
// request: R = units / D. It is the single source for every soft-slice quota
// (compute percent and per-card memory limit); the count of memory-limit entries
// follows the ".sliced" card count, not R.
func sliceRatio(ctr *core.Container, unitsResName core.ResourceName) (float64, error) {
	q, ok := ctr.Resources.Limits[unitsResName]
	if !ok {
		return 0, fmt.Errorf("container %q has no %s request", ctr.Name, unitsResName)
	}
	return float64(q.Value()) / float64(nodefeature.ResourceMaxUnits), nil
}
```
Companion helpers (same package): `floorPct(R) = int(R*100)` for the integer compute percent;
`runtimeMajor(rv, def) = split(rv, ".")[0]` falling back to `def` (`"12"` / `"8"`); `ascendFamilyDir(major,
family) = "cann-"+major+"-"+lower(family)` and `nvidiaCudaDir(major) = "cuda-"+major`. Both vendor responders
branch on `s.AllocationMode == workercore.DeviceAllocationModeSliced` — Exclusive/Shared keep the existing
`*_VISIBLE_DEVICES`-only response; the sliced branch keeps `*_VISIBLE_DEVICES` **and** adds preload + quota.

Conventions: return errors explicitly (a missing library path or request fails the allocate, never a silent
whole-card exposure); reconcile/GC idempotently and level-based; table-driven tests asserting the final
`ContainerAllocateResponse` (envs + mounts + on-disk side effects), not internal call order.

### Worked Example — container key and per-pod layout

For pod UUID `p-uuid` and container `train`, `X = p-uuid/c-train`. The allocator creates (mode 0777 unless
noted):
- `/tmp/vgpulock` (NVIDIA only)
- `/var/lib/gpustack/operator/pods/p-uuid/c-train`
- NVIDIA: `…/c-train/tmp/vgpu` (mounted to container `/tmp/vgpu`)
- Ascend: `…/c-train/etc/enpu/vcann-rt/npu_info.config` (mode 0644, mounted to container path)

Library mounts resolve `RuntimeVersion.Major` (and Ascend `Family`) against the staged host tree
`/var/lib/gpustack/operator/lib/{nvidia/cuda-<major>,ascend/cann-<major>-<family>}`.

### Implementation Plan

Ordered **Ascend-first, then NVIDIA** (per the planning directive); docker builder stages are added **one at a
time with a build-verify after each** — no rushing. Every task leaves the tree compiling and tests green.

**Phase 0 — Shared scaffolding**
- [x] **T1 — `copy-dir.sh` + `GPUSTACK_LIB_DIR`.** Add `pack/gpustack-operator/rootfs/usr/bin/copy-dir.sh`
  (recursive; create `<dst>` if absent; overwrite a destination file only when its `sha256` differs); add
  Dockerfile `ARG GPUSTACK_LIB_DIR=${GPUSTACK_CONF_DIR}/lib`; copy the script to `/usr/bin/copy-dir.sh` (0755)
  in the final stage. **Acceptance:** `docker run --rm` script test passes (missing-dir create / identical
  skip / changed replace); image builds. **Verify:** the F1 docker-run test; `docker buildx build`.
  **Files:** `pack/gpustack-operator/rootfs/usr/bin/copy-dir.sh`, `pack/gpustack-operator/Dockerfile`.
- [x] **T2 — quota/version helpers** in `pkg/deviceplugin`: `sliceRatio(ctr, unitsResName) (float64, error)` =
  `units/D`; `floorPct`, `runtimeMajor`, `ascendFamilyDir`, `nvidiaCudaDir`. **Acceptance:** worked-example
  table (1/8→R=0.125→pct 12; 1/4→25; empty Major→`cuda-12`/`cann-8`). **Verify:** `make test
  ./pkg/deviceplugin/...`, `make lint`. **Files:** `pkg/deviceplugin/helper.go(+_test)`.

*Checkpoint 0: helpers + copy-dir.sh green; no behavior change to existing allocators.*

**Phase 1 — Ascend packaging (incremental docker)**
- [ ] **T3 — vendor vcann-rt submodule** at `pack/gpustack-operator/external/ascend/libvnpu` (latest commit) +
  `.gitmodules`. Read the README/CMake for build deps (`dcmi/ascendcl/c_sec`, `ASCEND_HOME_PATH`).
  **Acceptance:** submodule clones; build entrypoint identified. **Verify:** `git submodule update --init`.
  **Files:** `.gitmodules`, submodule gitlink.
- [x] **T4 — CANN builder stages, added one by one.** (CANN 9 = `9.1.0-beta.1`, see Build Targets note.)
  Locally verified-built: `cannbuild-8-910b`, `cannbuild-8-910c`, `cannbuild-9-910b` (each emits
  `lib/libvruntime.so` 0644 + `tools/enpu-monitor` 0755). `cannbuild-9-910c` / `cannbuild-9-950` are
  byte-identical bar the base-image tag (a3/950 of the same release, tags confirmed to exist) and their local
  verification is **deferred to CI / a disk-rich host** — the local Docker disk filled and crashed mid-pull
  (~55-60 GB of CANN images won't fit here). Original task text:
  Add stage `cannbuild-8-910b`
  (`quay.io/ascend/cann:8.5.0-910b-ubuntu22.04-py3.11`) that cmake-builds `libvruntime.so` + `enpu-monitor`;
  **build-verify that stage** (`--target cannbuild-8-910b`, artifacts present). Then add `8-910c`
  (`…8.5.0-a3…`), `9-910b` (`…9.0.0-910b…`), `9-910c` (`…9.0.0-a3…`), `9-950` (`…9.0.0-950…`) **one at a time,
  build-verifying each**. Finally add the final-image `COPY` into `${GPUSTACK_LIB_DIR}/ascend/cann-{8,9}-{910b,
  910c,950}/{lib/libvruntime.so,tools/enpu-monitor}`. **Acceptance:** each stage builds; final image holds all
  five dirs with both artifacts. **Verify:** per-stage `docker buildx build --target …`; final-image `ls`.
  **Files:** `pack/gpustack-operator/Dockerfile`.
- [x] **T5 — Ascend `ld.so.preload` asset.** Add
  `pack/gpustack-operator/rootfs/etc/gpustack/lib/ascend/ld.so.preload` = `/opt/enpu/vcann-rt/lib/libvruntime.so`;
  final-stage copy to `/etc/gpustack/lib/ascend/ld.so.preload` (0644). **Acceptance:** present in image, mode
  0644. **Verify:** image inspect. **Files:** rootfs asset, `pack/gpustack-operator/Dockerfile`.

*Checkpoint 1: image builds end-to-end; `${GPUSTACK_LIB_DIR}/ascend` shows the five targets + preload.*

**Phase 2 — Ascend integration**
- [x] **T6 — DaemonSet host staging.** In `device-manager/daemonset.yaml`: mount host `/tmp`; add an init
  container running `copy-dir.sh /etc/gpustack/lib /var/lib/gpustack/operator/lib`. **Acceptance:** rendered
  manifest carries the `/tmp` mount + the init container. **Verify:** `helm template` render test.
  **Files:** `deploy/gpustack-operator/chart/templates/device-manager/daemonset.yaml`.
- [x] **T7 — Ascend Sliced server + `GetContainerAllocateResponse`.** Register the Sliced server in
  `ascend.New()` (gated on `!opts.NoSliced`, parallel to NVIDIA). Branch the responder on `Sliced`: with
  `X=<podUID>/c-<ctrName>`, create `pods/<X>` (0777); render `pods/<X>/etc/enpu/vcann-rt/npu_info.config`
  (0644) with `physical-npu-id`=`Accelerator.Index`, **lowest-free `virtual-npu-id`** (scan existing on-disk
  `pods/*/…/npu_info.config` for that physical id, take the lowest unused index from 0), `aicore-quota`=
  `floor(R*100)`, `memory-quota`=`floor(memMB*R)`, `shm-id`=`Accelerator.ID` with spaces→`-`,
  `scheduling-policy=2`; keep `ASCEND_VISIBLE_DEVICES`; mounts: preload→`/etc/ld.so.preload`(ro),
  `cann-<major|8>-<lower(family)>/{lib/libvruntime.so,tools/enpu-monitor}`→`/opt/enpu/vcann-rt/...`,
  config→container path, `/dev/shm`→`/dev/shm`. **Acceptance:** table tests on rendered config + mounts; two
  concurrent slices on one physical NPU get distinct `virtual-npu-id`. **Verify:** `make test
  ./pkg/devicemanager/allocator/ascend/...`, `make lint`. **Files:**
  `pkg/devicemanager/allocator/ascend/deviceplugin.go(+_test)`, `pkg/deviceplugin/helper.go`.

*Checkpoint 2: the Ascend sliced response is fully asserted by unit tests, vNPU-id uniqueness included.*

**Phase 3 — Shared per-pod GC (lands with Ascend, serves both vendors)**
- [x] **T8 — notifier `chan struct{}` → `chan []string` + dir GC.** Widen the `DevicesReconciler` notifier to
  carry the live pod-UUID list (empty/nil ⇒ none) on each reconcile; the Sliced `ResourceServer` scans
  `pods/<uuid>` on startup and removes a UUID after **3 consecutive** absences from the list; a list UUID
  absent on disk is tracked. **Acceptance:** unit test feeding successive lists incl. nil asserts removal only
  after 3 misses; existing `ListAndWatch` still emits responses on every notification for all modes.
  **Verify:** `make test ./pkg/deviceplugin/...`, `make lint`. **Files:** `pkg/deviceplugin/controller.go`,
  `pkg/deviceplugin/server.go(+_test)`.

**Phase 4 — NVIDIA packaging (incremental docker)**
- [ ] **T9 — vendor HAMi-core submodule** at `pack/gpustack-operator/external/nvidia/libvgpu` (latest commit).
  **Acceptance:** clones; `build.sh` entrypoint identified. **Verify:** `git submodule update --init`.
  **Files:** `.gitmodules`, submodule gitlink.
- [ ] **T10 — CUDA builder stages, one at a time.** Add `nvbuild-12`
  (`nvidia/cuda:12.9.2-cudnn-devel-ubi8`) → `libvgpu.so`, build-verify; then `nvbuild-13`
  (`nvidia/cuda:13.0.3-cudnn-devel-ubi8`), build-verify; final `COPY` to
  `${GPUSTACK_LIB_DIR}/nvidia/cuda-{12,13}/libvgpu.so`. **Acceptance:** each stage builds; both artifacts
  present. **Verify:** per-stage `--target`; final `ls`. **Files:** `pack/gpustack-operator/Dockerfile`.
- [ ] **T11 — NVIDIA `ld.so.preload` asset** = `/usr/local/vgpu/libvgpu.so`; final copy to
  `/etc/gpustack/lib/nvidia/ld.so.preload` (0644). **Acceptance:** present, 0644. **Verify:** image inspect.
  **Files:** rootfs asset, `pack/gpustack-operator/Dockerfile`.

*Checkpoint 3: image holds `nvidia/cuda-{12,13}/libvgpu.so` + preload.*

**Phase 5 — NVIDIA integration**
- [ ] **T12 — NVIDIA sliced `GetContainerAllocateResponse`.** Branch on `Sliced`: create `/tmp/vgpulock`
  (0777), `pods/<X>` (0777), `pods/<X>/tmp/vgpu` (0777); envs `CUDA_DEVICE_SM_LIMIT=floor(R*100)`,
  `CUDA_DEVICE_MEMORY_LIMIT_<i>=floor(memKi*R)` (one per `.sliced` card), `CUDA_DEVICE_MEMORY_SHARED_CACHE=
  /tmp/vgpu/cudevshr.cache`, keep `NVIDIA_VISIBLE_DEVICES`; mounts `/tmp/vgpulock`(rw),
  preload→`/etc/ld.so.preload`(ro), `cuda-<major|12>/libvgpu.so`→`/usr/local/vgpu/libvgpu.so`(ro),
  `pods/<X>/tmp/vgpu`→`/tmp/vgpu`(rw), `/dev/shm`→`/dev/shm`. **Acceptance:** table tests on envs + mounts +
  dir creation; entry count == `.sliced` card count. **Verify:** `make test
  ./pkg/devicemanager/allocator/nvidia/...`, `make lint`. **Files:**
  `pkg/devicemanager/allocator/nvidia/deviceplugin.go(+_test)`, `pkg/deviceplugin/helper.go`.

*Final Checkpoint: full image builds; Ascend + NVIDIA sliced responses unit-tested; Ascend exercised
on-hardware per the Test Plan; NVIDIA on-hardware E2E runs externally.*

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
- None — the Sliced scheduling/accounting chain is already shipped and tested; this spec only adds the
  runtime-isolation injection on top.

#### Unit tests
Table-driven; target date 2026-06-25:
- `pkg/deviceplugin`: `2026-06-25` - target `>=75%` (`sliceRatio`/`floorPct`/`runtimeMajor`/dir resolvers; the
  `chan []string` GC: startup scan, 3-miss sweep, nil-list handling).
- `pkg/devicemanager/allocator/ascend`: `2026-06-25` - target `>=70%` (`npu_info.config` render, lowest-free
  `virtual-npu-id`, `shm-id` space→`-` transform, mounts + dir creation).
- `pkg/devicemanager/allocator/nvidia`: `2026-06-25` - target `>=70%` (`CUDA_DEVICE_SM_LIMIT` /
  `CUDA_DEVICE_MEMORY_LIMIT_<i>` envs, shared-cache env, mounts, `/tmp/vgpulock` + `pods/<X>` dir creation).

#### Integration tests
- `copy-dir.sh` via `docker run --rm`: missing-dir create / identical-file skip / changed-file replace.
- `helm template` device-manager: host `/tmp` mount + the `copy-dir.sh` init container present.
- Incremental docker build per stage: each CANN stage (`cannbuild-8-910b` … `9-950`) and each CUDA stage
  (`nvbuild-12`, `nvbuild-13`) emits its artifact under `--target`; the final image's
  `${GPUSTACK_LIB_DIR}` layout is asserted.

#### e2e tests
- **Ascend (on hardware, no RuntimeClass dependency):** mock the NFD PCI label so the Ascend device-manager
  DaemonSet scales out; run it with `--no-fast-failed` so a detector miss does not exit the DaemonSet;
  construct `Devices` from `testing/sample/devices`; submit an `alpine`/`ubuntu` pod requesting a slice and
  assert the injected preload library, `npu_info.config`, and mounts land in the container as specified.
- **NVIDIA:** no NVIDIA machine here (no NVIDIA RuntimeClass) → device-manager E2E runs externally; locally
  covered by the unit tests + the incremental image-build verification only.

## Alternatives

- **Ascend `hami-vnpu-core` instead of `vcann-rt`** (report §4.3): same injection skeleton, has a ready-made
  device-plugin submodule+CI reference, but README claims only 910B and needs an in-container `limiter`
  daemon. Rejected for this spec — the task commits to `vcann-rt` (C single-`.so`, official, wider machine
  coverage, config-file driven, no side daemon); `hami-vnpu-core` stays a possible future parallel provider.
- **`LD_PRELOAD` env instead of `/etc/ld.so.preload`**: simpler to inject but doesn't reliably cover child
  processes and is easy for the workload to overwrite. Rejected per report §2.3.
- **Copy the library at `Allocate()` time instead of an init container**: couples library staging to the hot
  allocate path and re-copies per container. Rejected — stage once per node via the init container; allocate
  only mounts.
- **Hard-code quota from partitions/MaxPartitions**: rejected — quota is derived from the actual
  `.sliced.units` request so U>1 and multi-card requests scale correctly.

## Open Questions

_None outstanding — all clarifications resolved (path layout, round-down quotas, latest-commit submodules +
tagged base images, `chan []string` notifier payload, and the hyphen-joined `shm-id` / `scheduling-policy=2`
confirmed against the vcann-rt README)._
