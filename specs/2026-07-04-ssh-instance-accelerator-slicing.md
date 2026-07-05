# Spec: Colocate Sliced Accelerator Resources With the Instance Workload (SSH-Enabled Instances)

Status: Building
Type: Bug fix

## Summary
GPUStack Instances that enable SSH are rendered as a two-container Pod: a `main` container running the
user's image (CPU/RAM/storage sandbox) and an Alpine/musl `sshd` sidecar that provides SSH by `nsenter`-ing
into `main`. When such an Instance requests a **sliced** accelerator (soft-slicing via HAMi-core `libvgpu.so`
on NVIDIA, or vcann-rt `libvruntime.so` on Ascend), the slice silently does not take effect: `nvidia-smi`
and CUDA see the whole physical card instead of the requested fraction. Root cause: the device-plugin injects
all slicing prerequisites into the container that holds the sliced resource (the `sshd` sidecar), but the
user's workload actually executes inside `main`'s mount namespace — where the preload file and interception
library do not exist, so the interception library is never loaded. This change fixes the defect by
**co-locating the sliced accelerator resource, its injected artifacts, and the workload in the `main`
container**, granting the `sshd` sidecar only a **narrow device-cgroup permission** for the same physical
device, and **dropping excess Linux capabilities** in the SSH entry path before the interactive shell starts.
It also closes a confirmed host-escape security weakness in the current design. The fix is symmetric across
the NVIDIA and Ascend backends.

## Motivation
### Goals
Root cause (measured on AWS EKS + NVIDIA T4; confirmed at code level for both backends):
- `InstanceReconciler.convertPodFromInstance` builds `main` with general resources only and `sshd` with
  accelerator resources only ("main + sshd: main has general only, sshd has accelerator only"); the Pod sets
  `HostIPC: true` and `ShareProcessNamespace: true`.
- The device-plugin `Allocate` for the sliced resource injects, into the **requesting** container's response
  (i.e. `sshd`): mounts `/etc/ld.so.preload` → interception-library path, the interception library
  (`/usr/local/vgpu/libvgpu.so`), `/tmp/vgpulock`, a per-pod cache dir (`/tmp/vgpu`), `/dev/shm`; and env
  `NVIDIA_VISIBLE_DEVICES`, `CUDA_DEVICE_SM_LIMIT`, `CUDA_DEVICE_MEMORY_SHARED_CACHE`, per-card
  `CUDA_DEVICE_MEMORY_LIMIT_<i>`.
- The `sshd` sidecar's `ForceCommand` runs an entry script that finds the `main` process and
  `exec`s `nsenter --target <PID> --mount --uts --ipc --net --pid -- chroot <main-rootfs> setpriv
  --clear-groups <shell> -l`; the interactive shell (and any CUDA process from it) therefore runs in `main`'s
  mount namespace and rootfs.
- `/etc/ld.so.preload` is resolved by the dynamic linker relative to the process's own mount-namespace root
  filesystem. `main`'s rootfs (user image) has no preload file referencing the interception library and no
  interception library → it is never preloaded → NVML/CUDA return real values → the whole card is reported.
- Hard blocker = the interception library is not preloaded in `main`. The absent limit env is secondary and
  is moot while the library is not loaded.
- Measured: T4 16 GiB, 60% mem / 100% cores → injected limit `CUDA_DEVICE_MEMORY_LIMIT_0=9830m`; through the
  real SSH path `nvidia-smi` reports `15360 MiB` (whole card). Injecting the library + preload file + limit
  env into `main` flips the reported total to `9830 MiB` and the interception library becomes mapped in the
  process — a controlled reverse experiment that closes the causal chain.

Fixed behavior (testable success criteria):
- A sliced Instance's workload observes the slice whether it runs as `main`'s own process or via SSH:
  `nvidia-smi --query-gpu=memory.total` ≈ the sliced fraction (~9830 MiB for 60% of a 16 GiB T4), and CUDA
  allocations beyond the limit fail with out-of-memory.
- The Ascend equivalent (vcann-rt) enforces the NPU memory/compute quota for the workload in `main` and via SSH.
- An SSH user of the Instance cannot escalate to host-level access (cannot create/read host block devices,
  cannot mount the host filesystem) from the interactive shell.

Target users: GPUStack end users running sliced GPU/NPU workloads (as the container process and interactively
over SSH); cluster operators multi-tenanting a physical accelerator via soft-slicing who rely on the slice
being enforced and the node not being exposed to SSH users.

### Non-Goals
- Changing HAMi-core / vcann-rt internals or the enforcement algorithm.
- Adding new allocation modes (Exclusive/Shared/Sliced unchanged).
- Redesigning the Instance model beyond the sliced-accelerator + SSH device-access path.
- Migrating off the classic kubelet Device Plugin API to Dynamic Resource Allocation (DRA) — noted as a
  long-term alternative, out of scope here.
- Cross-pod / cluster-wide accounting semantics (Kueue admission unchanged).
- Non-Linux nodes; cgroup-v1-only special casing (delegated to the OCI runtime, which handles both).

## Proposal
Co-locate the sliced accelerator resource, the device-plugin-injected artifacts, and the user workload in the
**same** container (`main`); give the `sshd` sidecar only what it needs to `nsenter` into `main` and to let
the interactive shell reach the same physical device — without excess privilege.

Desired outcome (no implementation details):
1. The sliced accelerator request is on `main`, so the device-plugin injects the preload file, interception
   library, lock/cache, and limit env / quota file into `main`'s namespace/rootfs/env — where the workload runs.
2. The `sshd` sidecar gets a **narrow device-cgroup permission** for the *same* physical device `main` was
   allocated (visible-devices env resolved to the allocated device, never a wildcard, never privileged).
3. The SSH entry path enters `main` (needs the capability to `nsenter`) then **drops excess capabilities**
   before executing the login shell, so the interactive user does not inherit the sidecar's powers.

### User Stories
#### Story 1 — Sliced GPU visible over SSH
As a user who requested a 60%-memory GPU slice on an SSH-enabled Instance, I want `nvidia-smi`/CUDA in my SSH
session to report the sliced memory, so that I can trust the isolation.
- Steps: create a sliceable Instance (`acceleratorSlicedMemoryPercentage: 60`, `…CoresPercentage: 100`,
  `sshPublicKey` set) → wait Ready → SSH in.
- Expected: `nvidia-smi --query-gpu=memory.total` ≈ 9830 MiB (not 15360); the interception library is mapped
  in the process.
#### Story 2 — Business workload in `main` is sliced
As a user running a GPU service (e.g. vLLM) as the Instance's main process, I want that process to observe
the slice, so that a co-located tenant cannot be starved.
- Steps: create a sliceable Instance whose image/command runs the GPU workload directly (no SSH needed).
- Expected: the workload's NVML/CUDA view reflects the slice; allocation beyond the limit is rejected.
#### Story 3 — Memory limit enforced (not just reported)
As an operator, I want allocations beyond the slice to fail, so that slicing is a real guard.
- Steps: in the sliced session, allocate more device memory than the slice.
- Expected: allocation fails with out-of-memory at the slice boundary.
#### Story 4 — SSH user cannot escape to the host
As a security stakeholder, I want an SSH user confined, so that a tenant cannot read/modify the host node.
- Steps: SSH in → attempt to `mknod` a host block device and read the host root filesystem → inspect the
  shell's effective capabilities.
- Expected: `mknod` denied (Operation not permitted); host root disk unreadable; the shell's effective
  capability set is empty (or a documented minimal set); GPU access still works.
#### Story 5 — Ascend NPU slice via SSH
As an Ascend user, I want the same guarantees on NPU, so that behavior is backend-consistent.
- Steps: create a sliceable Ascend Instance → SSH in → run the NPU status tool / a quota-bound allocation.
- Expected: the NPU memory/compute quota is enforced for the SSH-launched workload; device access works.
#### Story 6 — Multiple slices coexist on one physical card
As an operator, I want two slices of one physical accelerator to run within the advertised budget.
- Steps: schedule two sliced Instances whose combined memory-percentage ≤ 100 onto the same card.
- Expected: both admit and run; each observes its own slice.

### Core Features & Acceptance Criteria
- F1 — Workload/resource co-location: the sliced resource lands on the workload container (`main`), so injected
  artifacts are in the workload's mount namespace/rootfs/env. Acceptance: for an SSH-enabled sliced Instance,
  `main`'s rootfs has the preload file + interception library and `main`'s env carries the limit variables;
  `nvidia-smi` in `main` reports the slice.
- F2 — Narrow device authorization for the sidecar: `sshd` gets a device-cgroup permission for the *same*
  physical device (scoped, not wildcard, not privileged). Acceptance: the SSH session opens the device and
  reports the slice; a negative control without the grant fails to initialize the device.
- F3 — Capability hardening of the SSH entry path: the interactive shell launches with excess capabilities
  dropped, after the `nsenter` step. Acceptance: the shell's effective/bounding cap sets are empty (or a
  documented minimal set); device access + slicing still work; host `mknod`/mount is denied.
- F4 — Backend parity (NVIDIA + Ascend): the same co-location + narrow-authorization applies to Ascend
  vcann-rt (quota via mounted `npu_info.config`; `libvruntime.so` + `libdcmi`/CANN deps present because `main`
  is the resource holder). Acceptance: an Ascend sliced Instance enforces its quota for both the `main` process
  and the SSH session.

### Notes / Constraints / Caveats
- Operator conventions: reconcile desired state, level-based, idempotent, typed errors, context propagation.
- The device-plugin `Allocate` response applies mounts/envs/devices only to the requesting container (K8s
  device-plugin API) — this is why moving the request moves the artifacts.
- Device access needs BOTH: the device node visible in the process's current mount namespace, AND a
  device-cgroup allow rule for its major/minor. The allow rule is produced by the container runtime from the
  *selected device* — NVIDIA Container Toolkit (CDI: edits the OCI spec, adds `Linux.Devices` +
  `Linux.Resources.Devices`, runc enforces; legacy: `nvidia-container-cli` writes it directly) and Ascend
  Docker Runtime (`addDeviceToSpec` appends `Linux.Devices` + a `LinuxDeviceCgroup{Allow:true,…,"rwm"}` and
  lets runc enforce). It is NOT produced by bind-mounting `/dev`; mounting `/dev` grants no access. This is also
  what closes the historical reason the accelerator was placed on `sshd` ("after `nsenter` from `sshd`, GPU
  devices were not visible"): the sidecar's **own** device-cgroup grant — not holding the accelerator resource —
  is what makes `main`'s devices reachable from the SSH shell, confirmed by the PoC negative control (sidecar
  without the grant → NVML/NPU init fails).
- `nsenter` does not change cgroup membership; joining `main`'s cgroup from inside the sidecar is not feasible
  (measured: `nsenter --join-cgroup` fails under a private cgroup namespace, and `cgroup.procs` is read-only).
  So device permission must come from the sidecar's own device-cgroup grant.
- Interception libraries are glibc-built; the `sshd` sidecar is Alpine/musl, which ignores `/etc/ld.so.preload`
  and cannot load a glibc `.so` — another reason artifacts must live in `main` (typically glibc).
- Ascend specifics: `ASCEND_VISIBLE_DEVICES` does not accept `all` (explicit index/range only); the quota
  travels in a mounted `npu_info.config` (not env); `libvruntime.so` needs `libdcmi` (host-injected into the
  resource holder) + CANN libs — satisfied naturally when `main` holds the resource.
- **kubelet Device-Plugin Allocate model** (verified against kubernetes/kubernetes; shapes F2's co-allocation):
  - The `Allocate` RPC is **anonymous** — `AllocateRequest`/`ContainerAllocateRequest` carry only device IDs,
    no pod/container identity. A plugin cannot learn the pod/container from the RPC; GPUStack infers it
    heuristically (`getAllocatingPod` picks the **oldest** pending pod on the node whose container `Limits`
    match the resource name + exact quantity — **no node lock**). For the Instance's own two containers this is
    tractable in-process — the pod-watcher and Allocate handler are the same on-node process
    (`pkg/devicemanager/controllers/setup.go`), and kubelet serializes Allocate per-node, non-interleaved, in
    Pod spec order (see Phase 2.2) — so co-allocation needs **no distributed lock**. Only the separate case of
    two **distinct identical** pending pods keeps the heuristic fragile; HAMi/Volcano-VGPU harden *that* with a
    lock + scheduler-written reservation, and if GPUStack ever needs the same it should use a per-node
    `coordination.k8s.io` Lease (GPUStack namespace), not the hot Node object (see Risks and Alternatives) — but
    it is out of scope for this fix.
  - Allocate is issued **one RPC per container**, **sequentially** during pod admission, **init containers
    first then regular containers in Pod spec order**, and **non-interleaved across pods** (kubelet holds a
    mutex). Because the operator emits `main` before `sshd`, `main` is **always allocated before** `sshd` — so
    correlation is one-directional (main records, sshd reuses) with no risk of `sshd` being allocated first and
    no need to block-wait inside an Allocate call.
  - Sharing the **same physical device across a pod's containers is supported** (kubelet reuses devices already
    allocated to a pod's containers; the plugin also chooses the injected UUID in its Allocate response).
  - Allocate happens before container creation/cgroup programming, and the result is **not modifiable by an
    external controller** (a pod-annotation round-trip after allocation is not a supported hook) — so
    correlation must happen inside the plugin, not in the operator after the fact.
- Kueue admission and the sliced request shape (`.sliced`, `.sliced.cores-percentage`,
  `.sliced.memory-percentage`, `.sliced.units`) must keep working; only the carrying container changes.

### Boundaries
- **Always:** keep sliced resource + injected artifacts + workload in the same container; drop excess
  capabilities before the interactive shell; scope the sidecar's device permission to the allocated device.
- **Ask first:** any change to the sidecar image contract (`instance-ssh-server-image`) or to device-plugin
  co-allocation/affinity semantics; any widening of the sidecar's capabilities; whether to keep `HostIPC: true`
  (host IPC exposure) vs. a pod-level IPC namespace.
- **Never:** run any Instance container privileged; grant a wildcard device permission when the specific
  allocated device is known; rely on bind-mounting `/dev`; leave the interactive shell with
  `CAP_SYS_ADMIN`/`CAP_MKNOD`/`CAP_SYS_PTRACE`.

### Risks and Mitigations
- Moving the resource to `main` requires the sidecar to still reach the same physical device → co-allocate that
  device to the sidecar as device-only permission; single-accelerator node = trivial (sole device, no
  correlation), multi-accelerator = needs same-device affinity between the Instance's two containers.
- Allocate ordering (would `sshd` be allocated before `main`?) → **resolved by construction**: kubelet
  processes containers in Pod spec order, sequentially and non-interleaved; the operator emits `main` before
  `sshd`, so `main` is always allocated first and the sidecar only ever *reuses* `main`'s device.
- Anonymous Allocate RPC (no pod/container identity) → for the Instance's **own two containers** this is
  resolved in-process (Phase 2.2): the pod-watcher and the Allocate handler are the **same on-node process**
  (`pkg/devicemanager/controllers/setup.go` — one `DevicesReconciler`), and kubelet serializes Allocate
  per-node, non-interleaved, in Pod spec order, so `main`'s Allocate records its device in an in-process
  pod-keyed reservation that `sshd`'s device-only Allocate reuses in the same window. Writing the reservation
  *inside* `main`'s Allocate (not from an external watcher) removes the watcher→Allocate happens-before race;
  the `device.gpustack.ai/accelerator.allocated` annotation carries crash recovery. → **No distributed lock is
  required for co-allocation** (difficulty reduced from HIGH to MEDIUM).
- Separate, pre-existing identity risk (**out of scope for this fix**) → when two **distinct but identical**
  Instances are pending on the same node, `getAllocatingPod`'s oldest-quantity-match heuristic can misattribute
  a device. This is the problem HAMi/Volcano-VGPU harden with a lock + scheduler-written reservation (their
  optimistic `resourceVersion` writes alone proved insufficient). If it ever becomes load-bearing, the candidate
  is a **per-node `coordination.k8s.io` Lease** (GPUStack namespace, `holderIdentity` = pod `ns/name/uid` +
  device) — NOT a Node annotation, since the Node object is hot (NFD, kubelet status, cloud controllers) and
  kubelet's own NodeLease (KEP-589) moved heartbeats off it for exactly this contention reason; GPUStack already
  ships Lease client machinery. Tracked in Open Questions, not implemented here.
- Pod webhook forbids a Pod that mixes accelerator **modes** (`pkg/worker/webhooks/worker/pod.go`
  `ValidateCreate` → `podAcceleratorModes(pod).Len() > 1` is `Forbidden`; `acceleratorMode` classifies by
  resource-name suffix + `IsKnownAcceleratableResourceName`) → if `sshd`'s device-only grant uses a **known**
  acceleratable resource name, `main`(sliced) + `sshd`(exclusive) reads as two modes and admission fails.
  Mitigation: grant the sidecar device permission via a resource name **outside** the known-acceleratable
  families (so `acceleratorMode` returns `""`), or teach the webhook to exempt the sidecar device-only resource;
  add a unit test for the mixed Pod.
- Host IPC exposure (orthogonal, pre-existing) → the Pod sets `HostIPC: true` (`instance.go`), so both containers
  share the **host** IPC namespace (host SysV IPC / `/dev/shm`) — a broader exposure than the caps/device-cgroup
  threat model behind Story 4 addresses. `nsenter --ipc` only needs to enter `main`'s IPC namespace, which a
  **pod-level** IPC namespace already provides. Mitigation: evaluate dropping `HostIPC` in favor of the default
  pod IPC namespace and confirm the SSH/`nsenter` path still works; if `HostIPC` must stay, document why in the
  threat model.
- Ascend has no wildcard visible-devices → device-plugin/operator must propagate the exact allocated index to
  the sidecar. With the in-process reservation this reuses the same mechanism as NVIDIA (the index is recorded
  at `main`'s Allocate); the only Ascend-specific extra is that even the single-accelerator case needs an
  explicit index (no `all` shortcut).
- Pod webhook may assume the `.sliced` request is on a specific container → confirm `pkg/worker/webhooks/worker/pod.go`
  triggers on the `kueue.x-k8s.io/queue-name` label (container-agnostic) and add a unit test with the request
  on `main`.
- Dropping all caps could break a workload needing a specific cap (e.g. `CAP_IPC_LOCK`) → GPU/NPU access needs
  no capability (it comes from the device-cgroup grant), so the SSH shell drops all caps by default; a
  configurable retained-cap allowlist was considered and deferred as unneeded — revisit only if a real workload
  proves it needs a specific cap.
- The `nsenter` step needs `CAP_SYS_ADMIN` → keep caps only through `nsenter`; drop at the final exec of the
  user shell.
- Regression in the exclusive/whole-card SSH path → keep the two-container arch; change only where the
  accelerator resource + device permission land; keep the exclusive path under e2e.

## Design Details
### Commands
- Lint: `make lint` (also runs via the post-Go-change hook).
- Codegen after API/webhook changes: `make generate`.
- Unit tests: `make test`.
- Vendored deps: `make deps`.
- e2e reproduction (manual): `helm install gpustack-operator deploy/gpustack-operator/chart -n gpustack-system
  --create-namespace --set image.tag=dev --set cleanupOnUninstall=true`; create a sliceable SSH Instance; then
  `kubectl exec` / `kubectl port-forward` + `nvidia-smi --query-gpu=memory.total --format=csv` to observe the
  slice (expect ~9830 MiB, not 15360).
### Project Structure
- `pkg/worker/controllers/worker/instance.go` — Instance→Pod (`convertPodFromInstance`: builds `main` then
  `sshd`, so `main` precedes `sshd` in `Pod.Spec.Containers`; `getResourceRequirements` flag matrix;
  `HostIPC`/`ShareProcessNamespace`; sidecar image resolution).
- `pkg/devicemanager/allocator/nvidia/deviceplugin.go`, `.../ascend/deviceplugin.go` — sliced Allocate response
  (`getSlicedContainerAllocateResponse`); the new device-only response for the sidecar lands here.
- `pkg/deviceplugin/server.go` — `Allocate` (device IDs come from kubelet; the injected UUID is chosen here),
  `GetPreferredAllocation`, `ListAndWatch`; `controller.go` — `getAllocatingPod` (heuristic pod match),
  `patchAllocatingPod` + the `device.gpustack.ai/accelerator.allocated` annotation; `helper.go` — sliced
  resource-name/units helpers.
- `pkg/worker/webhooks/worker/pod.go`, `instance.go` — sliced defaulting/validation.
- `pack/ssh-server/{Dockerfile,rootfs/chroot.sh,rootfs/entrypoint.sh}` — the sidecar image (SSH entry path).
- `api/worker/v1alpha1/devices.go` — `DeviceAllocationMode` (Sliced == 3).
### Code Style
The load-bearing split to change lives in `getResourceRequirements`/`convertPodFromInstance`:
```go
// TODAY — main: general only; sshd: accelerator only
Resources: getResourceRequirements(inst, instType, /*withGeneral*/ true, overcommit, /*withAccelerator*/ false),
Resources: getResourceRequirements(inst, instType, /*withGeneral*/ false, false, /*withAccelerator*/ true),
// AFTER — main: general + accelerator (workload + slicing artifacts); sshd: device-only permission (F2)
```
Conventions: reconcile to desired state (no imperative one-shot), typed errors returned early with actionable
conditions, contexts propagated, watch only relevant resources, table-driven tests with fake clients.
### Implementation Plan
Legend: [op]=operator (pkg/worker), [dp]=device-plugin (pkg/deviceplugin, pkg/devicemanager), [img]=ssh-server
image (pack/ssh-server), [t]=tests. Difficulty noted per task. Every phase leaves the system working.

#### Phase 0 — Codify the reproduction (test-first)
- [ ] 0.1 [t] Add an e2e reproduction for an SSH-enabled sliced NVIDIA Instance asserting the FIXED behavior
      (SSH in → `nvidia-smi --query-gpu=memory.total` ≈ slice, not whole card). Difficulty: low. AC: the case
      exists and runs; on current code it is RED (reports whole card). Verify: run it against a sliceable T4
      node; confirm the whole-card failure documents the bug.
- Checkpoint: the defect is codified as an executable test.

#### Phase 1 — [op] Co-locate the sliced accelerator on the workload container (`main`)
- [x] 1.1 In `convertPodFromInstance` (needSSHD branch), give `main` general **and** accelerator resources and
      make `sshd` carry no general/accelerator resources — flip the `getResourceRequirements` flags: `main` →
      `(withGeneral=true, …, withAccelerator=true)`, `sshd` → device-only (Phase 2). Preserve `HostIPC` +
      `ShareProcessNamespace` and the `main`-before-`sshd` container order. Difficulty: medium. AC: rendered
      Pod places `.sliced*` on `main`; device-plugin injects `/etc/ld.so.preload` + `libvgpu.so` +
      `CUDA_DEVICE_*` into `main`. Verify: create a sliced SSH Instance; `kubectl exec -c main -- cat
      /etc/ld.so.preload` present; `kubectl exec -c main -- nvidia-smi` reports the slice.
- [x] 1.2 [op] Confirm/adjust the Pod webhook (`pkg/worker/webhooks/worker/pod.go`) sliced defaulting +
      validation still fire with the request on `main` (it keys on the `kueue.x-k8s.io/queue-name` label,
      container-agnostic). Difficulty: low. AC: `.sliced.units`/cores defaulting unchanged; admission passes.
      Verify: webhook unit test with the sliced request on `main`; e2e admission succeeds.
- Checkpoint: **Story 2 passes** (a workload run as `main`'s process is sliced). SSH-path GPU access is
      temporarily unavailable for **both** sliced and exclusive Instances — moving the accelerator off `sshd`
      removes its device-cgroup grant, and the SSH shell runs in `sshd`'s cgroup; Phase 2 restores it. This is an
      intermediate state on the feature branch (main-process GPU access works for both), not a shippable point —
      1.1 → 2.x land together before merge. The non-SSH (main-alone) path is unaffected.

#### Phase 2 — [op+dp] Grant `sshd` a narrow device permission for `main`'s physical device (unified device-plugin co-allocation)
Design (decided over the earlier single-card/multi-card split): the operator cannot scope the sidecar to a
specific device at Pod-render time — the Pod is not scheduled yet, so no node/UUID/index is known, and Ascend
has no `all` wildcard — so the scoping must happen on-node in the device-plugin `Allocate`, which is the only
place the selected device is known. `sshd` requests a new device-only **visibility** resource
`device.gpustack.ai/<manufacturer>.visibility`, with the **same quantity** `main` asks of the real accelerator
resource (its card count — the value of `<vendor>/<device>[.shared|.sliced]`), so the sidecar is co-allocated
visibility to the same physical device(s). The name is **outside the known-acceleratable families** (so the Pod
webhook's one-mode check ignores it — see Risks). It is served by the **existing `ResourceServer`** running under a
new **internal-only** `DeviceAllocationMode` (`Visibility`) — not a separate server type — advertised (via
`Resource.GetDeviceIds`) as a **per-card pool of `SlicedResourceMaxSize` tokens** mapping to no real device
selection, so it never gates scheduling. Its device-plugin `Allocate` does not select a fresh device: it reads the physical device(s) `main` was already
allocated and returns **only the vendor visible-devices env pointing at those devices** (`NVIDIA_VISIBLE_DEVICES=<main's device ids>`;
Ascend `ASCEND_VISIBLE_DEVICES=<indexes>`) — no HAMi preload/limit, no slice consumed. The container runtime
derives the device nodes + device-cgroup from that env exactly as it does for the existing exclusive/shared
response (which is itself just a visible-devices env). `main` is always allocated before `sshd` (Phase 1.1 order
+ kubelet per-container, sequential, non-interleaved, spec-order Allocate), so `main`'s device is known when
`sshd`'s Allocate runs. This unified path covers single- and multi-accelerator nodes and both backends; it
needs no distributed lock (same-process pod-watcher + Allocate handler — `pkg/devicemanager/controllers/setup.go`).
- [x] 2.1 [dp] Add the internal-only `DeviceAllocationMode` (`Visibility`, appended after `Sliced`) and map its
      resource name **through the single `GetAcceleratableResourceName(manufacturer, mode)`** (no separate
      helper): `Visibility` → `device.gpustack.ai/<manufacturer>.visibility`, deliberately **outside** the
      known-acceleratable families so `IsKnownAcceleratableResourceName` returns false for it. The mode is never
      advertised on an InstanceType and never written to Devices status. Difficulty: low. AC:
      `IsKnownAcceleratableResourceName(visibilityName)` is false and the Pod webhook's
      `acceleratorMode(visibilityName)` returns `""` (a `main`(sliced) + `sshd`(visibility) Pod stays
      single-mode). Verify: unit tests in `pkg/nodefeature` and `pkg/worker/webhooks/worker`.
- [x] 2.2 [dp] Add an **in-process, pod-keyed reservation** on the singleton `DevicesReconciler`: `main`'s
      Allocate records its allocated `DevicesStatus` (device ID + Index) keyed by pod UID; the visibility
      Allocate reads it. Evict **on pod delete** via the existing `Reconcile` live-pod-UID sweep (not on consume,
      so a kubelet Allocate retry still resolves); a no-op for an empty UID/allocation, so it cannot leak. Writing
      the reservation *inside* `main`'s Allocate removes any watcher→Allocate race; the persisted
      `device.gpustack.ai/accelerator.allocated` annotation remains the durable/crash-recovery read fallback.
      Difficulty: medium. AC: record→read→prune behave under table-driven tests; `main`'s Allocate records the
      reservation; concurrent-pod safety via the reconciler mutex. Verify: `pkg/deviceplugin` unit tests.
- [x] 2.3 [dp] Serve the visibility resource from the **existing `ResourceServer`** under the internal
      `Visibility` mode (2.1). No new server type. Touch points, all keyed on `AllocationMode == Visibility`:
      (a) `GetResourceName` needs no branch — it already delegates to `GetAcceleratableResourceName`, which maps
      `Visibility`; (b) advertising folds into `Resource.GetDeviceIds` → a per-card pool of `SlicedResourceMaxSize`
      tokens (the most slices, hence sidecars, a card can host), not tied to a real device selection, so it never
      gates scheduling and consumes no `.sliced` ledger units; (c) `GetPreferredAllocation` → no preference (skip
      the sliced bin-fit); (d) `Allocate` → identify the pod (`getAllocatingPod`, quantity = `main`'s card count),
      read `main`'s device(s) from the reservation (2.2), and **fail closed** if the reservation is empty (error,
      never emit an empty visible-devices env a runtime could read as "all"); it skips `patchAllocatingPod`/ledger
      and delegates to the Responder. The NVIDIA Responder needs **no change**: for any non-`Sliced` mode it
      already returns `NVIDIA_VISIBLE_DEVICES=<ids>` only, so the reserved device ids flow straight through.
      Register the visibility server in `nvidia.New()`. Difficulty: medium. AC: the response points `sshd` at
      exactly `main`'s device(s); no HAMi artifacts; no ledger unit consumed; each card advertises
      `SlicedResourceMaxSize` visibility tokens; a **missing or stale** reservation (the reserved device no
      longer in the node inventory) fails closed — never delegating an empty visible-devices env. Verify:
      `pkg/deviceplugin` + `pkg/devicemanager/allocator/nvidia` unit tests on the visibility ListAndWatch/Allocate
      + fail-closed + response shape; multi-GPU e2e. (Ascend registers its visibility server in Phase 4.)
- [x] 2.4 [op] In `convertPodFromInstance`, make `sshd` request the visibility resource
      `device.gpustack.ai/<manufacturer>.visibility` with the **same quantity as `main`'s accelerator card count**
      (replacing the "device-only (Phase 2)" placeholder from 1.1). Difficulty: low. AC: rendered `sshd` carries
      the visibility resource with quantity = `main`'s card count; the webhook admits the mixed Pod (2.1); `main`
      still carries the `.sliced` request. Verify: `instance_test.go` unit test; e2e admission.
- [x] 2.5 [dp] Register the visibility server in **every** accelerator backend's `New()`, not only NVIDIA/Ascend
      (review-driven). The operator emits `device.gpustack.ai/<manufacturer>.visibility` for *any* acceleratable
      manufacturer (2.4 is gated only on `requestAccelerator`), so a backend that does not advertise it would
      leave its SSH accelerator Instances permanently `Pending`. The visibility server is manufacturer-agnostic
      (its Allocate reuses `main`'s reservation and delegates to the backend Responder, whose non-sliced path
      already emits the plain device-visibility response — vendor env for NVIDIA/Ascend/AMD/Cambricon/Iluvatar/
      MThreads, device nodes for Hygon/MetaX/THead). Difficulty: low. AC: `amd`, `cambricon`, `hygon`, `iluvatar`,
      `metax`, `mthreads`, `thead` each register `DeviceAllocationModeVisibility`; an SSH exclusive Instance on any
      backend schedules. Verify: `go build`; existing allocator tests stay green.
- Checkpoint: **Story 1 passes** on single- and multi-accelerator nodes (hardware e2e). Out of scope — the
      *separate*, pre-existing identity problem (two **distinct but identical** Instances pending on the node at
      once, where `getAllocatingPod`'s oldest-quantity-match heuristic can misattribute) is orthogonal to this
      two-container co-allocation and is **not addressed by this fix** (tracked in Open Questions; a
      `coordination.k8s.io` Lease is the candidate hardening only if it becomes load-bearing).

#### Phase 3 — [img] Capability hardening of the SSH entry path (independent; PoC-verified)
- [x] 3.1 In `pack/ssh-server/rootfs/chroot.sh`, drop all capabilities at the final exec of the user shell
      (after `nsenter`): `setpriv --clear-groups --bounding-set=-all --inh-caps=-all` (no retained-cap
      allowlist — device access + slicing come from the device-cgroup grant, not a capability). Difficulty: low.
      AC: SSH shell `CapEff` empty; device access + slicing still work; host `mknod` denied. Verify: SSH in →
      `grep CapEff` (empty) + `nvidia-smi` (slice) + `mknod` host dev (EPERM). Build/push the image; set
      `instance-ssh-server-image`.
- Checkpoint: **Story 4 passes**; slicing unaffected.

#### Phase 4 — [op+dp] Ascend parity
- [x] 4.1 Apply resource-on-`main` + `sshd` device-only co-allocation to Ascend; ensure `ASCEND_VISIBLE_DEVICES`
      carries the **exact allocated index** to `sshd` (no `all`); `main` (resource holder) gets `libvruntime.so`
      + `npu_info.config` + runtime-injected `libdcmi`/CANN. Difficulty: MEDIUM — but the operator side (2.4,
      manufacturer-agnostic via `GetAcceleratableResourceName`), the in-process reservation (2.2) and the
      visibility Allocate (2.3, delegating to the Responder) already generalized, so this reduced to
      **registering the Ascend visibility server in `ascend.New()`**: the responder already emits only
      `ASCEND_VISIBLE_DEVICES=<indexes>` (exact index, no `all`, no vcann-rt artifacts) for every non-sliced
      mode, so the reserved index flows straight through. AC: an Ascend sliced SSH Instance enforces its NPU
      quota for the `main` process and the SSH session. Verify: Ascend e2e where hardware exists; else unit tests
      on the ascend allocator visibility response + a documented manual runbook.
- Checkpoint: **Story 5 passes** (backend parity).

#### Phase 5 — [t] Regression + edge coverage
- [ ] 5.1 Regression: exclusive/whole-card SSH Instance still works. Verify: e2e.
- [ ] 5.2 Multi-slice coexistence within budget (**Story 6**). Verify: e2e.
- [ ] 5.3 Memory OOM enforcement beyond the slice (**Story 3**). Verify: e2e allocation test.
- Checkpoint: full green across NVIDIA (+ Ascend where hardware allows).

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
- e2e infra must offer a sliceable NVIDIA node (T4/g4dn): `deploy/gpustack-operator/chart` install with
  `image.tag=dev` + a sliceable InstanceType (validated on EKS). Ascend NPU e2e requires hardware not in
  current CI → covered by unit tests + a documented manual runbook.
- Existing `instance.go` / webhook / allocator unit tests that assert "accelerator on sshd, general on main"
  must be updated to the new split (accelerator on `main`, `sshd` device-only).

#### Unit tests
- `pkg/worker/controllers/worker` (instance.go): `getResourceRequirements` places accelerator on `main`;
  `convertPodFromInstance` renders `sshd` as device-only, keeps `main` before `sshd`, retains
  `HostIPC`/`ShareProcessNamespace`. Target: `2026-07-04` - maintain ≥ current package coverage.
- `pkg/worker/webhooks/worker` (pod.go, instance.go): sliced defaulting/validation with the request on `main`;
  `ValidateCreate` admits the mixed `main`(sliced) + `sshd`(device-only) Pod (the sidecar's device-only resource
  name is outside the known-acceleratable families, so `podAcceleratorModes` stays at one mode). Target:
  `2026-07-04` - maintain ≥ current.
- `pkg/devicemanager/allocator/nvidia` + `.../ascend`: `getSlicedContainerAllocateResponse` unchanged for the
  workload container; new device-only response for the sidecar; co-allocation returns `main`'s device
  (NVIDIA UUID / Ascend index). Target: `2026-07-04` - maintain ≥ current.
- `pkg/deviceplugin` (server.go, controller.go): Allocate correlation for the sidecar device-only request via
  the in-process pod-keyed reservation recorded at `main`'s Allocate and reused by `sshd`'s Allocate in the same
  pod window; annotation-based crash recovery; reservation eviction on consume/pod-delete. Target: `2026-07-04`
  - maintain ≥ current.

#### Integration tests
- Operator envtest: Instance→Pod rendering — accelerator on `main`, `sshd` device-only, `main` before `sshd`,
  `HostIPC=true`, `ShareProcessNamespace=true`; webhook admission of a sliced SSH Instance. Concrete test names
  added after the implementation PR merges.

#### e2e tests
- e2e-story1: sliced SSH NVIDIA Instance → SSH → `nvidia-smi` memory.total ≈ slice (~9830 MiB for 60% T4).
- e2e-story2: sliced Instance, workload as `main` process → sliced.
- e2e-story3: allocation beyond the slice → out-of-memory.
- e2e-story4 (security): SSH user → `mknod`/host-root read denied; shell `CapEff` empty; GPU still works.
- e2e-story6: two slices on one card within budget both run.
- e2e-regression: exclusive/whole-card SSH Instance unaffected.
- Ascend (story5): justification — no NPU in CI; unit tests + a manual runbook instead.

## Alternatives
Three approaches were built/evaluated on EKS:

| Approach | Business (main process) sliced | SSH-shell sliced | ssh-server image change | operator change |
|---|---|---|---|---|
| A. Status quo (resource on `sshd`) | No (whole card via image `NVIDIA_VISIBLE_DEVICES=all`) | No (whole card) | — | — |
| B. Keep resource on `sshd`, propagate artifacts+env into `main` at SSH time | No (main's own process still whole card) | Yes | Requires sidecar image change | None |
| C. **Recommended** — resource+artifacts+workload on `main` + narrow sidecar device permission | **Yes** | **Yes** | **None (stock sidecar works)** | container-assignment + sidecar co-allocation |

- Rejected — `nsenter --join-cgroup` to give the SSH shell `main`'s cgroup permission: measured to fail under a
  private cgroup namespace + read-only cgroupfs.
- Rejected — privileged sidecar: functionally grants GPU (allow-all device cgroup) but is a host-escape hole —
  the SSH shell inherits full capabilities (the entry script clears groups, not caps) and can `mknod`+read the
  host root disk. Never use.
- Rejected — bind-mount `/dev` into the sidecar: adds node visibility only; the device cgroup still denies →
  `open()` EPERM.
- Rejected (for this fix) — mutating admission webhook to set the sidecar's visible-devices: the allocation
  (and thus the device UUID) is decided by kubelet *after* webhooks run, so a webhook cannot know the UUID.
- Precedent (recommended reference for Phase 2.2) — HAMi / Volcano-VGPU **NodeLock** (`hami.io/mutex.lock`):
  the scheduler acquires a node lock at Bind (before patching the Pod's device-reservation annotation), the
  anonymous device-plugin Allocate reads the lock to learn which pod it is serving, and releases after all
  devices are allocated / on Bind failure / a 5-minute timeout (`HAMI_NODELOCK_EXPIRE`). It serializes
  allocation node-wide because the real deduction is deferred to the async Allocate — their optimistic
  `resourceVersion` annotation writes alone proved insufficient. This is the proven hardening for the
  `getAllocatingPod` identity problem. **But HAMi's lock spans the scheduler→on-node-Allocate boundary because
  HAMi's custom scheduler selects the device and must hand it to the anonymous on-node Allocate; GPUStack selects
  the device on-node inside Allocate, and its pod-watcher and Allocate handler are the same process
  (`pkg/devicemanager/controllers/setup.go`) — so the Instance's two-container co-allocation needs no distributed
  lock at all** (Phase 2.2 uses an in-process pod-keyed reservation). A Lease would only matter for the separate,
  out-of-scope case of two **distinct identical** pods contending on one node; if that is ever hardened, carry
  the reservation in a **per-node `coordination.k8s.io` Lease** (GPUStack namespace,
  `holderIdentity` = pod `ns/name/uid` + device, native expiry via `leaseDurationSeconds`/`renewTime` vs HAMi's
  manual timestamp + 5-min timeout), **not a Node annotation** — the Node object is hot (NFD, kubelet status,
  cloud controllers) and kubelet's own NodeLease (KEP-589) moved heartbeats off it for exactly this reason;
  GPUStack already ships Lease client machinery + Lease-based leader election.
- Related direction (separate, larger refactor) — moving sliced **counting** to NFD-advertised node-level
  extended resources counted by the default-scheduler (deduct up front via the scheduler assume cache) would
  remove any cluster lock from the *counting* layer (only an in-node placement mutex remains). It does not by
  itself solve the *device-injection* Allocate identity problem (Phase 2.2), which stays anonymous regardless;
  tracked apart from this fix.
- Deferred — Dynamic Resource Allocation (DRA / ResourceClaim): its API carries pod/claim identity and supports
  sharing a claim across a pod's containers, which would cleanly solve the anonymous-Allocate correlation
  problem. It is a large migration off the classic device-plugin API and is out of scope here; recorded as the
  long-term direction if multi-accelerator co-allocation (Phase 2.2) proves too fragile.

## Open Questions
- In-process reservation lifecycle for the two-container co-allocation (Phase 2.2): the exact pod-keyed key
  (pod `uid` + physical device — NVIDIA UUID / Ascend index), where it lives on the singleton
  `DevicesReconciler`, and how it is evicted (consumed by `sshd`'s Allocate; swept on pod delete by the sliced
  `ListAndWatch` GC that already tracks live pod UIDs) so a crashed or never-arriving `sshd` Allocate cannot
  leak or mis-bind a reservation.
- The separate, out-of-scope identity problem (two **distinct identical** pending pods → `getAllocatingPod`
  oldest-match can misattribute): if it must be hardened later, confirm the reservation object is a per-node
  `coordination.k8s.io` Lease (not a Node annotation) and settle its **acquisition point** (no scheduler-bind
  hook exists in the Kueue + default-scheduler flow — a scheduling gate/webhook vs the on-node device-manager
  watching pods bound to its node), **namespace/naming** (a GPUStack namespace, not `kube-node-lease`), and RBAC.
  Not implemented in this fix.
- Resolved: the sidecar carries a **formal device-only visibility resource**
  `device.gpustack.ai/<manufacturer>.visibility` (outside the known-acceleratable families, so admission reads
  one mode), requested with quantity = `main`'s card count. It is served by the existing `ResourceServer` under
  an internal `Visibility` allocation mode (no separate server type) and advertised per card as a pool of
  `SlicedResourceMaxSize` tokens mapping to no real device selection, so it never gates scheduling and consumes
  no `.sliced` ledger units; its anonymous Allocate is correlated by the same in-process, same-pod-window
  reservation and returns only `<vendor>_VISIBLE_DEVICES` for `main`'s device(s).
- Maintenance invariant (no automated guard): every accelerator backend registered in
  `supportedAllocatorCreators` must also register the `Visibility` server in its `New()`, because the operator
  emits the visibility resource for all acceleratable manufacturers. The `device.Allocator`/`Server` interfaces
  expose no resource-name enumeration, so this is not unit-testable without new production API; a future central
  registration helper (or an interface method) would make the omission impossible rather than merely conventional.
- Whether `HostIPC: true` can be dropped to a pod-level IPC namespace (the `nsenter --ipc` path only needs
  `main`'s IPC namespace); if it must stay, document the host SysV IPC / `/dev/shm` exposure in the threat model.
- Upgrade/rollback of already-running SSH Instances when the container-assignment + co-allocation change ships
  (Pods rendered under the old accelerator-on-`sshd` split): recreate the Instance's Pod vs. tolerate both shapes.
- Resolved: no configurable retained-capability allowlist — chroot.sh drops all capabilities
  (`--bounding-set=-all --inh-caps=-all`); GPU/NPU access needs none (it comes from the device-cgroup grant).
  Revisit only if a real workload proves it needs a specific capability.
- Cores/compute-throttle verification (only the memory slice was empirically verified; cores=100% did not
  exercise throttling).
- Whether F3 (capability hardening) ships in the same change or as a separately reviewable security fix.
