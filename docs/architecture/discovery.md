# Device Discovery

> **Purpose** — how a node's hardware becomes labels and a per-accelerator ledger: what NFD
> publishes, what the Device Manager detects, and what the device-plugin allocator does at `Allocate`.
> **Audience** contributors · **Prerequisites** [Architecture](../architecture.md) · **Read time** ~18 min

This is stages 1 and 2 of the four-stage chain. Stage 3 and 4 — turning these signals into Kueue
objects — are in [Scheduling Chain](scheduling-chain.md).

## Contents

- [Device, Accelerator, Resource](#device-accelerator-resource)
- [Stage 1: Node Feature Discovery (NFD)](#stage-1-node-feature-discovery-nfd)
- [The gpustack-cpu-info NodeFeatureRule](#the-gpustack-cpu-info-nodefeaturerule)
- [Stage 2: the Device Manager (DM)](#stage-2-the-device-manager-dm)
- [The Devices ledger](#the-devices-ledger)
- [The device-plugin allocator](#the-device-plugin-allocator)
- [Logical slicing per manufacturer](#logical-slicing-per-manufacturer)
- [SSH-enabled Instances and the visibility resource](#ssh-enabled-instances-and-the-visibility-resource)
- [Container identification and cross-mode exclusion](#container-identification-and-cross-mode-exclusion)
- [Placement is a preference, not a decision](#placement-is-a-preference-not-a-decision)
- [The partitioned family: fungible tokens](#the-partitioned-family-fungible-tokens)
- [One driver stack per node](#one-driver-stack-per-node)

## Device, Accelerator, Resource

Three words, one direction of dependency. Read the diagram as one sentence: *the Device Manager
manages Devices; Kubernetes consumes them as Resources.*

```
                     ┌────────────────────────────────────────────────┐
                     │  Device — what the node carries                │
   manages           │                                                │
 DeviceManager ─────▶│    ├── Accelerator   GPU / TPU / XPU / NPU     │
 (detector,          │    └── (future) IB port, Link port, …          │
  allocator)         │                                                │
                     └────────────────────────────────────────────────┘
                                        │
                                        │  maps onto
                                        ▼
                     ┌────────────────────────────────────────────────┐
   consumes          │  Resource — how Kubernetes sees a Device       │
 DevicePlugin ──────▶│                                                │
 Controllers         │    Resource{Group, Device}                     │
                     │    ResourceToken = Resource + Index            │
                     └────────────────────────────────────────────────┘
```

- **Device** — anything the node carries that the Device Manager manages.
- **Accelerator** — a Device usable for compute acceleration: GPU, TPU, XPU or NPU. It is the default
  word for the physical unit of accounting, and what the rest of these pages count.
- **Resource** — the Kubernetes-side view of a Device: what the device plugin and the controllers name
  when consuming one. The hardware layer never speaks it.
- **card** — a manufacturer-hardware term, used only where a manufacturer's SDK models a card as
  something other than exactly one Accelerator: the Ascend DCMI card that contains several devices,
  and the T-Head device-node ordinal (see [T-Head PPU Partitioning
  Operations](../operation/thead-mig.md)).
- **manufacturer** — the company. Its native code is *the manufacturer's library, SDK or binding*.

The `pkg/device` package doc carries the same diagram for a reader coming from the code.

## Stage 1: Node Feature Discovery (NFD)

NFD is deployed as the `node-feature-discovery` subchart, or brought by the cluster itself (see [Two
install modes](install-modes.md)). It performs three jobs.

### Job 1 — PCI vendor labels

Labels every Node that carries a PCI display/accelerator-class device (PCI classes `02`, `03`, `0b`,
`12`, from the chart value `node-feature-discovery.worker.config.sources.pci.deviceClassWhitelist`;
the rule below matches the same classes from `pkg/nodefeature`, and the DM's own sysfs scan reads
`GPUSTACK_PCI_CLASS_PREFIXES`, see [Settings & Environment Variables](../settings.md)) with:

```
feature.node.kubernetes.io/pci-${PCI_VENDOR_ID}.present: "true"
```

For example, a node with an NVIDIA device gets `feature.node.kubernetes.io/pci-10de.present: "true"`.

### Job 2 — CPU identity

Labels every Node with its CPU model identity (the `cpu` label source), and annotates it with the CPU
details through the `gpustack-cpu-info` NodeFeatureRule:

```
feature.node.kubernetes.io/cpu-model.vendor_id: "AMD"
feature.node.kubernetes.io/cpu-model.family:    "25"
feature.node.kubernetes.io/cpu-model.id:        "1"
```

```
feature.gpustack.ai/cpu-name:             "AMD EPYC 7763 64-Core Processor"
feature.gpustack.ai/cpu-family:           "25"
feature.gpustack.ai/cpu-physical-cores:   "64"
feature.gpustack.ai/cpu-threads-per-core: "2"
feature.gpustack.ai/cpu-logical-cores:    "128"
feature.gpustack.ai/cpu-stepping:         "1"
feature.gpustack.ai/cpu-cache-line:       "64"
feature.gpustack.ai/cpu-hz:               "2450000000"
feature.gpustack.ai/cpu-boost-freq:       "3500000000"
feature.gpustack.ai/cpu-cache-l1i:        "32768"
feature.gpustack.ai/cpu-cache-l1d:        "32768"
feature.gpustack.ai/cpu-cache-l2:         "524288"
feature.gpustack.ai/cpu-cache-l3:         "33554432"
```

An annotation keeps its `@cpu.model.*` template reference verbatim when NFD cannot resolve the
attribute, so values leading with `@` are treated as unreported.

**The general(CPU) node key.** The Worker later normalizes these into the node's general(CPU) node key
(`nodefeature.ExtractGeneralNodeKey`), which is always non-empty and **always blends the node's real
CPU identity** as `${cpuManufacturer}-${id}`: the id leads with the sanitized
`feature.gpustack.ai/cpu-name` annotation when reported (trademark markers and the trailing
`" CPU @ …"` frequency part are dropped, a leading manufacturer prefix is deduplicated, and the result
is truncated to fit the naming budget — e.g. `amd-epyc-7763`), or with the cpu-model family and id
labels as the fallback when the annotation is unavailable (e.g. `amd-25-1`). The manufacturer is the
lowercased `cpu-model.vendor_id` label — reported as a [cpuid](https://github.com/klauspost/cpuid)
vendor enum name, so `Intel` → `intel`, `AMD` → `amd` — falling back to `generic` when it is
unknown or unreported (and the whole key degrades to `generic` only when NFD reports no CPU identity
at all).

Whether the CPU manufacturer actually **subdivides** the scheduling pools is a separate, runtime
decision made at the aggregation layer by
[`instance-type-aware-cpu-manufacturer`](../settings.md#online-adjustable-settings) (see
[Scheduling Chain](scheduling-chain.md#naming-and-grouping)) — the node key itself is always
CPU-accurate so the finest-grained `ResourceFlavor`s can be re-grouped without rewriting them.

**The key deliberately does not encode os/arch.** Instead, os/arch is appended in full to every
ResourceFlavor / ClusterQueue / InstanceType **name** (`…-linux-arm64`, never an abbreviation) and
pinned explicitly on the ResourceFlavor's `spec.nodeLabels` (`kubernetes.io/os`,
`kubernetes.io/arch`).

> **Why** — this is a correctness safeguard, not cosmetics: the cpu-model family/id labels are
> independent numbering spaces on x86 (CPUID) versus arm64 (MIDR), so a small value like `25-1` can
> legitimately appear on both architectures. amd64 and arm64 binaries are not interchangeable, so
> their capacity must never pool into one flavor/queue. Keeping os/arch out of the key (while pinning
> it on the name + nodeLabels) also reclaims label-length budget the old abbreviated `-ln-x64` suffix
> consumed.

### Job 3 — the non-accelerated marker

Labels Nodes that have **no** accelerator device from any known manufacturer with:

```
feature.gpustack.ai/acceleratable: "false"
```

This forms an explicit contrast with the `acceleratable: "true"` label reported later by the Device
Manager, which also corrects false negatives if they occur.

## The `gpustack-cpu-info` NodeFeatureRule

Jobs 2 and 3 are the work of the `gpustack-cpu-info` **NodeFeatureRule**, which the worker applies at
startup in every install mode (see [The chart deploys workloads, the worker applies the custom
resources](install-modes.md#the-chart-deploys-workloads-the-worker-applies-the-custom-resources)).

Its two matcher lists come from facts that exist for other reasons:

- the PCI vendor IDs of the manufacturers the worker manages — `--manufacturer`, which the chart
  fills from `global.manufacturers`, the same map that drives the device-manager DaemonSets — so a
  manufacturer added there is labelled, detected and given a device manager in one edit;
- the PCI classes `pkg/nodefeature` calls acceleratable, which are the classes NFD is configured to
  publish.

Two Go tests hold the chart's `global.manufacturers` map and its `deviceClassWhitelist` equal to what
`pkg/nodefeature` says.

**The manufacturer map is where a manufacturer's whole identity lives**: one row per manufacturer
carrying its `pciVendorID`, the `resourceName` its device-plugin advertises, the `runtimeName` its
workloads run under, its `partitionKind`, and two `runtimeInjects*` facts about what a container
would be missing without that manufacturer's container runtime:

- `runtimeInjectsDriver` — the user-space driver reaches a container only through the runtime. This
  is why the NVIDIA and MThreads **device-managers** run under a RuntimeClass, while every other
  manufacturer's reads its management library from a hostPath mount.
- `runtimeInjectsDevices` — the allocator contributes no device node of its own, so the runtime is
  the only thing that turns an allocation into `/dev` entries inside the container. This is the
  **workload's** need alone: AMD sets it because its allocator returns no device spec and leaves
  injection entirely to `amd-container-runtime`, driven by the `AMD_VISIBLE_DEVICES` the allocator
  writes — yet the AMD device-manager still reads its library from `/opt/rocm` on the host and does
  not run under the class.

**Either** of the two — never `runtimeName` — decides which RuntimeClasses the chart creates
(`deviceManager.createRuntimeClasses`, and only where the class is absent or already this release's),
so the set is deliberately narrower than the manufacturers that merely state a `runtimeName`. The
fields answer different questions: `runtimeName` is the class the operator will *use*, while either
`runtimeInjects*` fact means the runtime's presence can be **inferred**, since a manufacturer missing
what it names cannot work at all unless the handler is registered. A manufacturer with neither
injects its own device nodes and needs no RuntimeClass, so creating one would break it: the operator
attaches a RuntimeClass whenever one exists, and the kubelet rejects a Pod naming a runtime nothing
configured. A class the manufacturer's own operator created is still used; the chart never conjures
one.

## Stage 2: the Device Manager (DM)

For each known manufacturer, a DaemonSet named `gpustack-operator-device-manager-${manufacturer}` is
created with a node selector on the NFD PCI label. For example, nodes labeled
`feature.node.kubernetes.io/pci-10de.present: "true"` receive a Pod from the
`gpustack-operator-device-manager-nvidia` DaemonSet.

These DaemonSets are normally rendered by the Helm chart itself (`deviceManager.enabled=true`, the
default); with `deviceManager.enabled=false` the chart renders no device-managers at all — it does not
hand that install back to the worker, and a worker deployed by this chart never installs applications
at runtime. The worker installs them only where no chart deploys the worker, see [Two install
modes](install-modes.md).

### Detection and the accelerator feature labels

Once running, the DM detect loop (`pkg/devicemanager/detector/detector.go`) periodically detects
accelerators and reports a NodeFeature object named `${NODE_NAME}-gpustack-device-manager` (owned by
the Node), whose labels are built by `nodefeature.ConstructAcceleratableNodeLabels`. Each detected
accelerator model is keyed by the accelerated device key `${aKey} = ${manufacturer}-${id}`, where `id`
is the product name sanitized to satisfy Kubernetes label naming rules:

| Label                                                                | Meaning                                                            |
|----------------------------------------------------------------------|--------------------------------------------------------------------|
| `${prefix}acceleratable=true`                                         | Node has usable accelerators; overrides the NFD `false` marker      |
| `acceleratable.${prefix}${manufacturer}=true`                         | Accelerator manufacturer                                            |
| `acceleratable.${prefix}${manufacturer}.driver-version=${dv}`         | Device driver version (omitted when undetected)                     |
| `acceleratable.${prefix}${manufacturer}.runtime-version=${rv}`        | Device runtime version (omitted when undetected)                    |
| `acceleratable.${prefix}${aKey}=true`                                 | Concrete device model marker                                        |
| `acceleratable.${prefix}${aKey}.product=${name}`        | Product name                                                        |
| `acceleratable.${prefix}${aKey}.memory=${memory}`       | Per-accelerator VRAM size, formatted at the largest binary unit (e.g. `16Gi`) |
| `acceleratable.${prefix}${aKey}.cores=${cores}`         | Accelerator core count                                              |
| `acceleratable.${prefix}${aKey}.count=${acc}`           | Number of accelerators of this model on the node                    |
| `acceleratable.${prefix}${aKey}.family=${family}`       | Product family (omitted when undetected)                            |
| `acceleratable.${prefix}${aKey}.comcap=${cc}` | Compute capability (omitted when undetected)                      |

where `prefix` is `feature.gpustack.ai/` — so the device labels live under the dedicated
`acceleratable.feature.gpustack.ai/` key namespace — and `manufacturer` is one of the manufacturers
supported by `pkg/devicemanager/detector` (NVIDIA, AMD, Ascend, Cambricon, Hygon, Iluvatar, MetaX,
MThreads, T-Head). The per-manufacturer PCI vendor IDs, resource names, and runtime class names can
all be overridden — see [Settings & Environment
Variables](../settings.md#per-manufacturer-overrides).

## The `Devices` ledger

The NodeFeature object is owned by the Node. The DM also reports a **`Devices` custom resource** named
after the node, stamped with the accelerator flavors' selector labels (the feature key +
`kubernetes.io/os|arch`) so the pool's queue can reverse-look-up its Devices.

> **Why it is owned by the Node** — a `Devices` is cluster-scoped, and the garbage collector resolves
> a cluster-scoped dependent's owner in the empty namespace, so a namespaced owner like the
> NodeFeature is unresolvable and would leave the object uncollected. Cluster-scoped to
> cluster-scoped, it is collected with the node it describes.

Its `.status` holds the per-accelerator **`AcceleratorAllocation` ledger** — every accelerator's
`mode` (free / exclusive / shared / sliced / partitioned) and `Remaining` credit budget, plus, for a
hardware-partitioned accelerator, its allocated and still-placeable partition profiles — which is
the **single authoritative accounting** of accelerator occupancy: it drives the InstanceType
four-view display *and* feeds the per-accelerator AdmissionCheck (see [Admission](admission.md)).

The DM keeps monitoring devices, re-detecting whenever the device set or health changes; a separate
`NodeDevicesReconciler` syncs the `gpustack.ai/managed` mark from the Node onto the same-named
`Devices` so the per-node DM never asserts a node-management decision it does not own.

## The device-plugin allocator

Alongside detection, the DM runs the **device-plugin allocator** (`pkg/devicemanager/allocator/<mfr>`,
on `pkg/deviceplugin`): it registers per-mode resources (exclusive / shared / sliced / partitioned)
with the kubelet and, on `Allocate`, returns the container injection and records the allocation into
the `Devices` ledger.

**An accelerator serves only the family its reported capability can back**: an unpartitioned
accelerator advertises the exclusive, shared and logical-sliced token pools, an accelerator in a
hardware partitioning mode advertises only the `.partitioned` pool, and a family's tokens are simply
absent from the other population — which is what stops the kubelet from handing a partition request
an accelerator that cannot host it.

### Exclusive and shared

The injection is just the device-visibility env (`NVIDIA_VISIBLE_DEVICES` / `ASCEND_VISIBLE_DEVICES` /
…).

### Partitioned

It materializes the requested hardware instance (NVIDIA MIG, or T-Head's own MIG-named partitioning)
on an accelerator it selects itself and injects only that instance — as device nodes rather than an
environment variable for T-Head, which has no container-runtime hook; see [Accelerator
Requests](../accelerator-requests.md), [NVIDIA MIG Operations](../operation/nvidia-mig.md) and
[T-Head PPU Partitioning Operations](../operation/thead-mig.md).

### Sliced (logical slicing)

It additionally applies **runtime isolation** with **decoupled compute and memory budgets**: the
compute (SM / aicore) budget comes from `.sliced.cores-percentage` (defaulting to 100 %) and the
VRAM budget from the per-accelerator memory request (`.sliced.memory-percentage` preferred over
`.sliced.memory-mib`, floored and capped at the accelerator VRAM), so a slice can cap SM
independently of VRAM.

**How that budget is applied differs by manufacturer** — every sliceable manufacturer has real
per-slice runtime isolation, but only four of them get it from a preload library:

| Manufacturer | Mechanism |
|---|---|
| NVIDIA, Iluvatar, Ascend, T-Head | preload library + per-container compute/VRAM quota — see [NVIDIA and Iluvatar](#nvidia-and-iluvatar), [Ascend](#ascend), [T-Head](#t-head) |
| AMD | preload library for VRAM, plus a hardware **compute-unit mask** the operator derives and ROCr enforces — see [AMD](#amd) |
| MThreads | `MTHREADS_QOS_*` env vars consumed by the host sGPU kmod — see [MThreads](#mthreads) |
| Hygon | a per-pod `vdev.conf` compute+VRAM cap — see [Hygon](#hygon) |
| MetaX | a sysfs `sgpu` subdevice — see [MetaX](#metax) |
| Cambricon | a cnDev sMLU profile + instance — see [Cambricon](#cambricon) |

It also quiets each preload library's verbose per-call logging by default — injecting
`LIBCUDA_LOG_LEVEL=0` (HAMi-core) / `ENPU_LOG_LEVEL=1` (vcann-rt) unless the workload already sets
that variable — so a normal run is not buried in interception-library noise. T-Head is injected
`LIBHGGC_LOG_LEVEL=1` and AMD `LIBVROCM_LOG_LEVEL=1` under the same never-overwrite rule, and `1`
rather than `0` because their levels are not HAMi-core's: `1` logs a line per *denial*, not per
intercepted call, and `0` would hide the diagnostics of a slice that is refusing every allocation.
That is already the library's own default — naming it keeps the level a property of the allocation
rather than a library default. On Ascend it injects one more, `ENPU_DSMI_HOOK=1` under the same
never-overwrite rule (which reads the container's own `env:` entries — an `envFrom:`-sourced value
is not visible to the allocator, so opting out that way needs an explicit `env:`): it enables a
vendored vcann-rt hook (`pack/gpustack-operator/external/ascend/vcann-rt/`) so the container's own
`npu-smi info` reports its HBM **quota** and the slice's usage instead of the whole accelerator —
the same mixed view NVIDIA already gives, where `nvidia-smi` shows the virtual VRAM total while
power and temperature stay accelerator-wide.

## Logical slicing per manufacturer

### NVIDIA and Iluvatar

NVIDIA and Iluvatar are started with the manufacturer's preload library — both HAMi-core
`libvgpu.so` (Iluvatar reuses it, corex being a CUDA-compatible layer) — activated via
`/etc/ld.so.preload`, plus per-container quota env `CUDA_DEVICE_SM_LIMIT` /
`CUDA_DEVICE_MEMORY_LIMIT_*`.

Iluvatar keeps the accelerator visible through `IX_VISIBLE_DEVICES` and relies on
`ix-container-runtime` to inject corex, so a sliced Iluvatar Pod must carry
`runtimeClassName: iluvatar` — without it the preloaded `libvgpu.so` finds no corex `libcuda.so.1`
to hook. Its HAMi-core-on-corex enforcement is verified at the symbol level against a real corex
driver but **not yet on Iluvatar hardware**, so the capability is advertised-and-injected but
hardware-unvalidated.

### Ascend

Ascend is started with the manufacturer's preload library — vcann-rt `libvruntime.so` — activated
via `/etc/ld.so.preload`, plus per-container quota through an `npu_info.config` carrying
`aicore-quota` / `memory-quota`.

**On Ascend the allocator also turns on the driver's container-share mode for each accelerator it is
about to hand a second tenant** — the `sliced`, `shared` and `visibility` modes, never `exclusive`,
which owns whole accelerators — reading the flag through `binding/dcmi` and writing it only when it
is off, so an accelerator already carrying a tenant costs one query.

Without it the driver admits a single container per device and the *second* pod on an accelerator
still starts, then dies inside the container at `acl.rt.set_device` with `507899`
(`ACL_ERROR_RT_DRV_INTERNAL_ERROR`), naming neither the accelerator nor the flag; an accelerator
whose flag cannot be set therefore fails `Allocate` with both, rather than admitting a pod that
cannot use its device.

Two properties are worth stating plainly:

- **Whole-accelerator allocation is unaffected.** Measured on a 910B2 in both flag states, an
  exclusive container starts, sees the accelerator's full VRAM, and opens the device identically.
- **The flag's one real effect is that the driver stops refusing a second container** — which is why
  `npu-smi` warns *"There are security risks when opening device sharing, Please ensure that only a
  single user uses the chip"* before setting it. Multi-tenancy on one chip is exactly what logical
  slicing is for, and what keeps it safe here is not that guard but GPUStack's own per-accelerator
  ledger (the cross-mode invariant below) plus vcann-rt's memory-quota enforcement, which caps a
  slice at its `memory-quota` rather than at the accelerator total.

The flag persists in the driver, so an accelerator that has hosted a tenant stays shareable until
the host is rebooted or an operator clears it with `npu-smi set -t device-share`.

### AMD

**AMD splits the two dimensions across two enforcers, and it is the only manufacturer that does.**
Memory is a preload library like the others — `libvrocm.so`, built from this repository's own
`csrc/` tree, activated through `/etc/ld.so.preload`, reading a per-accelerator
`VROCM_DEVICE_MEMORY_LIMIT_<i>` in bare MiB and keeping its accounting in a per-container region
named by `VROCM_LEDGER_PATH`. Compute is **not** a variable at all: ROCm enforces it in hardware
through `HSA_CU_MASK`, which ROCr reads while it initialises, before any preloaded code exists. So
the operator *derives* the mask — a closed-form calculation over the accelerator's own topology,
branching on its GPU architecture family — and injects it, and the library never sees it.

A sliced AMD container therefore carries one value under two names: `AMD_VISIBLE_DEVICES`, which the
container runtime reads to inject `/dev/kfd` and the accelerator's render node, and
`ROCR_VISIBLE_DEVICES`, which the ROCm user-space runtime reads to filter and order its agents. Both
are the accelerator's `GPU-<hex>` UUID. That ordering is the index space the other two
per-accelerator variables live in — `HSA_CU_MASK`'s `GPU_list` index and
`VROCM_DEVICE_MEMORY_LIMIT_<i>`'s `<i>` are both positions in the `ROCR_VISIBLE_DEVICES` list, never
physical ordinals — so the three are emitted together and must stay in step.

> **Why this one needs a probe.** A CU mask fails **open**: a mask ROCr rejects produces no error,
> no log line and no changed return code, and the container simply gets the whole accelerator.
> Nothing a user can read reports it — `rocm-smi` and `amd-smi` read sysfs and never see a mask at
> all. So the allocator mounts `rocm-cumask-check` beside the library: it runs a kernel, reads the
> physical units its own waves landed on, and exits `0` only if they are the units the mask asked
> for. `rocm-monitor`, mounted alongside, prints the memory quota and what is charged against it. A
> slice that behaves like a whole accelerator is one command away from being diagnosed, on a node
> nobody is watching.

Because the mask is quantised to the accelerator's own allocation atom, the **smallest requestable
percentage is a per-accelerator property** — 9 % on a 60 CU / 3 shader-engine part, 3 % on a
304 CU / 8 XCC one. A request below it is refused at allocation time with the accelerator's minimum
in the message, rather than rounded up into a ceiling nobody asked for; a request above it that does not
land on the atom is aligned **down**, and the allocator logs the percentage actually delivered.

> **Admission does not know that minimum yet.** The webhook validates `1`–`100` and nothing
> publishes the per-accelerator floor, so a request below it is admitted, scheduled, and then
> refused by the device plugin — the Pod fails to start and will keep failing. Until the floor is
> published, the request to keep in mind is the very small one: on an accelerator with many shader
> engines, single-digit percentages may not be servable at all.

### T-Head

T-Head is started with the manufacturer's preload library — the pair `hggc_quota.so` (enforcement) +
`hgml_dlsym_hook.so` (visibility) — activated via `/etc/ld.so.preload`, plus per-container quota env
`HGGC_DEVICE_SM_LIMIT` / `HGGC_DEVICE_MEMORY_LIMIT_*`, where the compute figure is emitted **even at
100 %** because that library refuses an accelerator whose figure is missing rather than reading
absence as "no cap".

T-Head's sliced response carries **no** visible-devices env: like its other modes it passes the
accelerator's own device node plus the two shared control nodes, and adds only the library mounts,
the quota env and a per-container directory for the ledger region under the pod working directory
(per container, because the region is addressed by container-local accelerator index).

On T-Head the visibility half is what makes the container's own `ppu-smi` report its quota rather
than the physical accelerator, by interposing `dlsym`. **A workload image that brings its own
`dlsym` interposer through `LD_PRELOAD` — processed before `/etc/ld.so.preload` — leaves that half
loaded but never entered**: the quota still applies, but `ppu-smi` shows the whole accelerator.
Nothing in the library can detect that, so it is a caveat rather than an error. A mounted
`ppu-monitor` reads the quota and usage of both dimensions out of the container's own ledger region
(`HGGC_LEDGER_PATH`), which is where the compute cap can be seen at all — no `ppu-smi` field carries
it.

### MThreads

`MTHREADS_QOS_*` env vars consumed by the host sGPU kmod — the compute share is a scheduling weight,
not a hard cap.

### Hygon

A per-pod `vdev.conf` — a `cores%`-derived CU bitmask + VRAM cap — mounted read-only at
`/etc/vdev/docker/` and read by the host DTK/hyhal runtime.

### MetaX

A sysfs `sgpu` subdevice — the allocator puts the accelerator in `sgpu` mode and writes a
`cores%`-derived compute quota + VRAM cap under a `fixed-share` scheduling class to
`/sys/bus/pci/devices/<BDF>/sgpu/create`, then injects `METAX_SGPUS` plus the accelerator device
nodes for the host MetaX runtime.

### Cambricon

A cnDev sMLU profile + instance — the allocator creates or reuses a profile with `mluQuota = cores%`
and `memorySize` set to the VRAM budget, instantiates a subdevice, and injects its device nodes
`/dev/cambricon_dev*` / `/dev/cambricon_ipcm*` / the instance node, with a `VIRTUAL_DEVICES` env
fallback for `--use-runtime` deployments since sMLU does not support CDI.

### Where the preload libraries come from

They are compiled into the operator image per runtime version (cloned inline at pinned commits — no
git submodule — built in the `xbuild-nvidia-cuda-*` / `xbuild-ascend-cann-*` Dockerfile stages,
scripts under `pack/gpustack-operator/external/{nvidia,ascend}`) and staged onto the host
(`/var/lib/gpustack/operator/lib`) by a device-manager **init container**; the allocator mounts the
matching library + a per-pod working directory into each sliced container and reclaims those
directories once their pods are gone.

**Iluvatar adds no build stage of its own** — its lib dir is filled by copying the
`xbuild-nvidia-cuda-12` HAMi-core `/out` a second time (corex exposes a CUDA-compatible
`libcuda.so.1`, so the same library serves it), one flat directory with no runtime-version subdivision.

**AMD needs exactly one build stage, and that is a property of the library rather than a shortcut.**
`libvrocm.so` links no ROCm object at all — every runtime entry point is resolved at load time instead
of at link time — so one artifact serves every ROCm version a workload container may bring, and
`${GPUSTACK_LIB_DIR}/amd/` is flat where `nvidia/` and `ascend/` carry a subdirectory per runtime
generation. It is built from this repository's own `csrc/amd/rocm-slicing-shim` tree, inside a ROCm
devel image chosen for its glibc floor rather than its ROCm version, and it ships with two readers
(`rocm-monitor`, `rocm-cumask-check`) the allocator mounts beside it. ROCm publishes no `aarch64` user
space, so the **`arm64` operator image carries no AMD shim** — and no AMD node either, since the
detector's own libraries do not load there.

**T-Head's pair is the exception on both counts.** It is built from this repository's own sources
(`csrc/thead/ppu-slicing-shim`, compiled by the `xbuild-thead-ppu` stage inside the manufacturer's
SDK image) rather than cloned from an upstream at a pinned commit, and it carries no runtime-version
subdirectory, because the PPU SDK lives in the workload container rather than in ours. That SDK is
distributed for `x86_64` only, so the **`arm64` operator image carries no PPU shim** — the detector
does not check for one, on the ground that a PPU only exists in an `x86_64` host.

## SSH-enabled Instances and the visibility resource

For an **SSH-enabled Instance** the workload runs in a two-container Pod (`main` = the user image,
`sshd` = an Alpine sidecar that `nsenter`s into `main`). The accelerator request and its
runtime-isolation artifacts are placed on `main` — where the workload (and the SSH shell, which
enters `main`'s namespaces) actually runs — while `sshd` requests an internal-only
`device.gpustack.ai/<manufacturer>.visibility` resource (quantity = `main`'s accelerator count).

The allocator serves that resource from the same `ResourceServer` under an internal `Visibility`
mode: its `Allocate` selects no fresh device but reuses the physical device(s) `main` was already
allocated — correlated by an in-process, pod-keyed reservation recorded at `main`'s `Allocate`
(kubelet allocates `main` before `sshd`, sequentially, in Pod spec order), falling back to the Pod's
durable `device.gpustack.ai/accelerator.allocated` annotation so a device-manager restart landing
between the two calls no longer strands the sidecar — and returns only the manufacturer's
visible-devices env, with no slicing artifacts and no ledger consumption.

**What that env names follows the owner's family**: the accelerator(s) `main` holds for an
exclusive/shared/sliced owner, but for a **partition-backed** owner the partition itself, never the
parent accelerator — the accelerator hosts other tenants' partitions too, and the sidecar's
allocation is a device-cgroup grant and nothing else (the SSH session `nsenter`s into `main` and
inherits its environment, so it needs no injection of its own). The trigger is the owner container's
own `.partitioned.<kind>-<profile>` request, which is in the Pod spec from the start; the identity
comes from the manufacturer responder's partition capability (`PhysicalSlicedResponder` — the same
interface that materializes a partition, so a responder able to carve one can always name it), which
reads the owner's durable node-local ownership record and proves the recorded instance still live
before naming it (see [NVIDIA MIG Operations](../operation/nvidia-mig.md#requesting-a-mig-instance)
and [T-Head PPU Partitioning Operations](../operation/thead-mig.md#requesting-a-partition)). A
responder without that capability, or one that cannot substantiate the identity, fails the admission
closed rather than widening the grant back to the accelerator.

The visibility resource is advertised per accelerator as a pool of `SlicedResourceMaxSize` tokens
outside the known-acceleratable families, so it never gates scheduling and admission never reads it
as a second accelerator mode. It is registered by every accelerator backend. The per-accelerator
AdmissionCheck (see [Admission](admission.md)) re-checks feasibility only **before** admission —
once a Workload is admitted its own allocation is already in the ledger, so re-evaluating it is
skipped to avoid counting a slice against itself.

## Container identification and cross-mode exclusion

The device-plugin `Allocate` call carries the assigned device IDs but not the pod identity, so the
allocator maps a call to the (pod, container) being admitted by matching the pending pods on the
node that request the resource: it drops the candidates this call could not actually serve on the
accelerators offered (a slice whose per-accelerator demand exceeds their remaining), skips a (pod,
container) that already holds a reservation, and takes the oldest of the survivors — the candidates
left are genuinely interchangeable.

The feasibility test **disambiguates, it does not gate**: it reads a ledger that lags reality, so an
all-infeasible candidate set falls back to the unfiltered oldest rather than failing a resolvable
request; admission belongs to the Pod webhook and the AdmissionCheck, upstream.

Every node's `Allocate`s run in that node's single device-manager process, so a per-node in-process
mutex serializes each workload `Allocate`'s *identify → cross-mode check → reserve* section (the
durable-annotation patch runs outside it). Reservations are keyed per **(Pod UID, container)** and
the durable `device.gpustack.ai/accelerator.allocated` annotation is a **map keyed by container
name**, so two containers of one group each holding a live claim are both recorded and both charged
— an entry charges its accelerator until its **Pod** is gone, matching the reclaimer and the
kubelet, which both scope a device to the Pod's life rather than the container's.

Together this achieves two things.

**(a) It enforces the per-accelerator exclusive/shared/sliced cross-mode invariant.** An accelerator
kubelet assigned that another mode already holds, per the ledger `Status` or the in-process
reservation, is refused with `FailedPrecondition`, so an exclusive tenant truly owns its accelerator
on every path, Kueue or raw. Prevention runs one stage earlier too: `ListAndWatch` keeps an
accelerator held in another mode advertised (removing tokens would strand kubelet's checkpointed
allocations) but reports its tokens **Unhealthy** — read from the ledger `Status` and the in-process
reservation, and pushed on the same reservation/release instant — so kubelet, which picks tokens
freely (its `GetPreferredAllocation` call *does* run under the default TopologyManager policy
`none`, but is merely advisory — the kubelet is free to ignore the returned set), can never hand a
held accelerator to an opposite-mode pod in the first place. The `Visibility` server is exempt
because the `sshd` sidecar's token must stay allocatable on the very accelerator its workload holds,
whatever mode that hold is.

**(b) It maps a batch of identical GPU Pods admitted together (e.g. by Kueue) one-to-one to distinct
pods**, so their annotations and the per-accelerator ledger stay correct instead of one pod being
double-attributed and another lost. The `sshd` visibility path re-finds its Pod's **non-self
accelerator allocation** — the in-process reservation first, the durable annotation second, both
resolved by the same owner pick — rather than using the reservation-skip; because the request rules
confine a Pod's accelerator claims to one container group, that owner is unambiguous.

## Placement is a preference, not a decision

**For the accelerator-bound families**, their tokens name an accelerator, so the kubelet's pick of a
token *is* the pick of an accelerator; the plugin only gets to order the candidates it offers back
from `GetPreferredAllocation`.

For a **logical slice** it offers the tokens of the **most-occupied accelerator that still fits** —
an accelerator already serving slices is preferred over a pristine one, ties broken by the
accelerator's position within its group so identical requests against identical state place
identically — so slices coalesce instead of each opening a fresh accelerator and stranding a node
whose every accelerator is partly used but none can host one large claim. That ordering is computed
**per `DevicesGroup`**, walking the groups in spec order, not across a node's groups at once.

And it stays a *preference*: the per-accelerator fit filter lives **only** in this advisory response
— `Allocate` refuses an accelerator another mode holds, but never one merely short of room — so an
accelerator's VRAM budget is respected exactly insofar as the kubelet consumes the hint, with no
backstop below it.

Two properties are load-bearing:

- every id returned must be one the kubelet actually offered — the full
  `<group>:<accelerator>:<token>` form, since an id the kubelet cannot match is discarded
  **silently** and the accelerator choice degrades to arbitrary;
- the call stays advisory by API contract, so under a restrictive TopologyManager policy the kubelet
  allocates the NUMA-aligned set before consulting the plugin at all.

## The partitioned family: fungible tokens

The **`Partitioned`** family is the one exception to accelerator-bound tokens. Its `Allocate` treats
the kubelet's device IDs as a *quantity* and chooses the accelerator itself, under the same mutex,
against the live geometry — publishing the choice (accelerator, profile, intended memory-slice
intervals) into the reservation before releasing the mutex, so a concurrent call selects against
post-decision state.

Accelerators are **packed, not spread**: the most-occupied accelerator that still fits wins, which
keeps a sibling whole for a later whole-accelerator profile. A retried `Allocate` for a container
that already has an allocation reuses the accelerator it already used, read from the reservation and
then from the durable annotation.

Because no partition token names an accelerator, that family's health is a pure node-level count of
remaining room — `allocated + remaining` published over a stable set of IDs, never removing an ID a
live allocation holds — and the family reports **no** NUMA topology, since the kubelet would
otherwise align CPU and memory to an accelerator the plugin may not use.

One residual is stated rather than solved: a partition an administrator carves out of band is
invisible to every annotation-derived key, so hand-carving on a managed node is unsupported (see
[Accelerator Requests](../accelerator-requests.md#limitations)).

## One driver stack per node

A node hosts a single driver/runtime stack per manufacturer, so every `DevicesGroup` of a given
manufacturer on that node shares the same driver and runtime version. The per-runtime-version
library subdir (`cuda-<major>` / `cann-<major>-<family>`) the allocator picks from the first
allocated accelerator is therefore correct for every accelerator in a sliced allocation.

**Nothing below the detector re-checks that such a subdir was actually built**, so on Ascend the
detector offers logical slicing only for the family/runtime-major pairs the image ships a vcann-rt for
— one per `xbuild-ascend-cann-<major>-<family>` stage — and a pair with no stage is simply not
advertised, instead of being advertised and then failing to start the container on a missing
directory. Adding a build stage means widening that set.

The allocator nonetheless guards against a mismatch **defensively** (NVIDIA rejects a sliced
allocation spanning different CUDA majors; Ascend rejects a multi-accelerator sliced allocation,
since vcann-rt's `npu_info.config` models a single physical NPU), so any future regression fails the
`Allocate` loudly instead of silently mounting an incompatible library.

---

**See also** — [Accelerator Requests](../accelerator-requests.md) (the resource keys these families
serve) · [NVIDIA MIG Operations](../operation/nvidia-mig.md) · [T-Head PPU Partitioning
Operations](../operation/thead-mig.md) · [Settings](../settings.md)

**Next** → [Scheduling Chain](scheduling-chain.md) — how these labels and the ledger become Kueue
objects.
