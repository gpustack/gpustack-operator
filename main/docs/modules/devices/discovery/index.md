# Device Discovery

Node Feature Discovery publishes hardware labels, and the Device Manager records each accelerator
in `Devices`. These are stages 1 and 2 of the operator's discovery and scheduling chain.
[Scheduling Chain](/gpustack-operator/main/docs/modules/devices/scheduling/index.md) covers capacity profiling and queue creation in stages 3 and 4.

## Contents

- [Device, Accelerator, Resource](#device-accelerator-resource)
- [Stage 1: Node Feature Discovery (NFD)](#stage-1-node-feature-discovery-nfd)
- [Stage 2: the Device Manager (DM)](#stage-2-the-device-manager-dm)
- [The device-plugin allocator](#the-device-plugin-allocator)
- [Logical slicing per manufacturer](#logical-slicing-per-manufacturer)
- [SSH-enabled Instances and the visibility resource](#ssh-enabled-instances-and-the-visibility-resource)
- [Container identification and cross-mode exclusion](#container-identification-and-cross-mode-exclusion)
- [Placement preference](#placement-preference)
- [One driver stack per node](#one-driver-stack-per-node)

## Device, Accelerator, Resource

Three words, one direction of dependency: *the Device Manager manages Devices; Kubernetes consumes
them as Resources.*

```
                ┌────────────────────────────────────┐
   manages      │ Device — what the node carries     │
 DeviceManager ▶│  ├── Accelerator  GPU/TPU/XPU/NPU  │
 (detector,     │  └── (future) IB port, Link port…  │
  allocator)    └────────────────────────────────────┘
                            │ maps onto
                            ▼
                ┌────────────────────────────────────┐
   consumes     │ Resource — how Kubernetes sees it  │
 DevicePlugin  ▶│  Resource{Group, Device}           │
 Controllers    │  ResourceToken = Resource + Index  │
                └────────────────────────────────────┘
```

- **Device** — anything the node carries that the Device Manager manages.
- **Accelerator** — a Device usable for compute acceleration: GPU, TPU, XPU or NPU. The default word
  for the physical unit of accounting, and what these pages count.
- **Resource** — the Kubernetes-side view of a Device, named by the device plugin and controllers.
  The hardware layer never speaks it.
- **card** — a manufacturer-hardware term, used only where a manufacturer's SDK models a card as
  something other than exactly one Accelerator: the Ascend DCMI card holding several devices — a level
  the [V2 DCMI API](#ascend-two-dcmi-api-generations) drops entirely — and the T-Head device-node
  ordinal (see [T-Head MIG Operations](/gpustack-operator/main/docs/modules/devices/thead-mig/index.md)).
- **manufacturer** — the company. Its native code is *the manufacturer's library, SDK or binding*.

## Stage 1: Node Feature Discovery (NFD)

NFD comes from the `node-feature-discovery` subchart, or from the cluster itself (see [Installation Modes](/gpustack-operator/main/docs/operate/installation-modes/index.md)). It performs the following jobs.

### Job 1 — PCI vendor labels

Labels every Node carrying a PCI display/accelerator-class device with:

```
feature.node.kubernetes.io/pci-${PCI_VENDOR_ID}.present: "true"
```

An NVIDIA device gives `feature.node.kubernetes.io/pci-10de.present: "true"`. The whitelisted classes
are `02`, `03`, `0b` and `12`, set in the chart's
`node-feature-discovery.worker.config.sources.pci.deviceClassWhitelist`; the Device Manager's own
scan reads the same classes, overridable through `GPUSTACK_PCI_CLASS_PREFIXES` (see
[Settings](/gpustack-operator/main/docs/reference/settings/index.md)).

### Job 2 — CPU identity

Labels every Node with its CPU model identity (the `cpu` label source), and annotates the CPU details
through the `gpustack-cpu-info` NodeFeatureRule:

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

An annotation NFD cannot resolve keeps its `@cpu.model.*` template reference verbatim, so a value
leading with `@` counts as unreported.

The Worker normalizes these into the node's general(CPU) node key, never empty and always carrying
the node's real CPU identity, as `${cpuManufacturer}-${id}`:

- `cpuManufacturer` — the lowercased `cpu-model.vendor_id` label; `generic` when unknown or
  unreported.
- `id` — the sanitized `feature.gpustack.ai/cpu-name` annotation: trademark markers and the trailing
  `" CPU @ …"` frequency part dropped, a leading manufacturer prefix deduplicated, truncated to the
  naming budget (e.g. `amd-epyc-7763`); unreported, the cpu-model family and id labels instead (e.g.
  `amd-25-1`).
- The key degrades to `generic` only when NFD reports no CPU identity at all.

Whether that manufacturer subdivides the pools is a separate runtime decision, taken by
[`instance-type-aware-cpu-manufacturer`](/gpustack-operator/main/docs/reference/settings/index.md#online-adjustable-settings) (see
[Scheduling Chain](/gpustack-operator/main/docs/modules/devices/scheduling/index.md#naming-and-grouping)); the key stays CPU-accurate so the
finest-grained `ResourceFlavor`s regroup without rewriting.

The key deliberately does not encode os/arch. os/arch is appended in full to every ResourceFlavor,
ClusterQueue and InstanceType name (`…-linux-arm64`, never abbreviated) and pinned on the
ResourceFlavor's `spec.nodeLabels` (`kubernetes.io/os`, `kubernetes.io/arch`).

### Job 3 — the non-accelerated marker

Labels Nodes with **no** accelerator device from any known manufacturer with:

```
feature.gpustack.ai/acceleratable: "false"
```

It contrasts explicitly with the `acceleratable: "true"` the Device Manager reports later, which also
corrects false negatives.

### The `gpustack-cpu-info` NodeFeatureRule

The worker applies the `gpustack-cpu-info` NodeFeatureRule at startup in every installation mode.
It performs jobs 2 and 3 above; see [Installation
Modes](../../operate/installation-modes.md#chart-deployed-and-worker-applied-resources).

The rule marks PCI vendors managed by the worker as acceleratable. The chart's
`global.manufacturers` map registers each manufacturer for labeling, detection and its Device
Manager deployment.

## Stage 2: the Device Manager (DM)

For each known manufacturer a DaemonSet `gpustack-operator-device-manager-${manufacturer}` is created
with a node selector on the NFD PCI label: a node labeled
`feature.node.kubernetes.io/pci-10de.present: "true"` gets a Pod from
`gpustack-operator-device-manager-nvidia`.

The Helm chart normally renders them (`deviceManager.enabled=true`, the default). With
`deviceManager.enabled=false` it renders none and does not hand the install back to the worker: a
worker deployed by this chart never installs applications at runtime. The worker installs them only
where no chart deploys it, see [Installation Modes](/gpustack-operator/main/docs/operate/installation-modes/index.md).

### Detection and the accelerator feature labels

The Device Manager periodically detects accelerators and reports a NodeFeature
`${NODE_NAME}-gpustack-device-manager`, owned by the Node. Each model is keyed by the accelerated
device key `${aKey} = ${manufacturer}-${id}`, `id` being the product name sanitized to Kubernetes
label naming rules:

| Label | Meaning |
|---|---|
| `${prefix}acceleratable=true` | Node has usable accelerators; overrides the NFD `false` marker |
| `acceleratable.${prefix}${manufacturer}=true` | Accelerator manufacturer |
| `acceleratable.${prefix}${manufacturer}.driver-version=${dv}` | Device driver version (omitted when undetected) |
| `acceleratable.${prefix}${manufacturer}.runtime-version=${rv}` | Device runtime version (omitted when undetected) |
| `acceleratable.${prefix}${aKey}=true` | Concrete device model marker |
| `acceleratable.${prefix}${aKey}.product=${name}` | Product name |
| `acceleratable.${prefix}${aKey}.memory=${memory}` | Per-accelerator VRAM size, at the largest binary unit (e.g. `16Gi`) |
| `acceleratable.${prefix}${aKey}.cores=${cores}` | Accelerator core count |
| `acceleratable.${prefix}${aKey}.count=${acc}` | Number of accelerators of this model on the node |
| `acceleratable.${prefix}${aKey}.family=${family}` | Product family (omitted when undetected) |
| `acceleratable.${prefix}${aKey}.comcap=${cc}` | Compute capability (omitted when undetected) |

`prefix` is `feature.gpustack.ai/`, so device labels live under the dedicated
`acceleratable.feature.gpustack.ai/` key namespace. `manufacturer` is one of the supported names:
NVIDIA, AMD, Ascend, Cambricon, Hygon, Iluvatar, MetaX, MThreads, T-Head — their PCI vendor IDs,
resource names and runtime class names all overridable (see
[Settings](/gpustack-operator/main/docs/reference/settings/index.md#per-manufacturer-overrides)).

### The `Devices` ledger

The NodeFeature is owned by the Node. The `Devices` custom resource is named after the node, stamped
with the accelerator flavors' selector labels (the feature key +
`kubernetes.io/os|arch`) so the pool's queue can reverse-look-up its Devices, and is
garbage-collected with the Node it describes.

The worker also deletes it directly when an uncached
read finds the Node gone, so a departed node's ledger stops counting its cards sooner than the
collector's backoff would allow.

Its `.status` holds the per-accelerator **`AcceleratorAllocation` ledger**: each accelerator's `mode`
(free / exclusive / shared / sliced / partitioned) and `Remaining` credit budget, plus, for a
hardware-partitioned one, its allocated and still-placeable partition profiles.

For a logically sliced accelerator it also counts the slices it hosts, `allocatedSlices`, one per
container. An accelerator hosts at most its `logicalSliced.count` of them — 4 on Hygon — whatever its
`Remaining` says. This is the **single authoritative accounting** of accelerator occupancy, driving the
InstanceType four-view display *and* feeding the per-accelerator AdmissionCheck (see
[Admission](/gpustack-operator/main/docs/modules/devices/admission/index.md)).

The Device Manager re-detects whenever the device set or health changes, and the worker syncs the
`gpustack.ai/managed` mark from the Node onto the same-named `Devices`, so the per-node Device
Manager never asserts a node-management decision it does not own.

> **A pass that failed is not a manufacturer with no accelerators** — every detector answers an absent
> driver, a library that will not initialise and a bus holding no card with an empty list and no error.
> So an error means that pass could not measure, and carries no claim about the hardware. The loop
> reports such a manufacturer as it was **last detected**: its allocator keeps serving, the `Devices`
> keeps its group and the node keeps that family's capacity keys. A manufacturer that has never
> answered is still absent, and a pass that ran and found nothing is what undetects one, which also
> drops whatever was held for it, so a later failure cannot resurrect a card that was pulled.
>
> The monitor pass cannot be carried forward the same way, because a sample is worth what its
> timestamp claims. A manufacturer it could not measure is simply absent from the sample, and is named
> as unmeasured so that its absence is not read as accelerators that went away — which would take the
> loop round again on no evidence.

> **Before the first detection, a Device Manager asked for one manufacturer reports nothing at
> all**: no
> `Devices`, no NodeFeature, nothing published to the allocator. Its DaemonSet is scheduled by that
> manufacturer's PCI vendor label, so a node that answers with no accelerators is one whose driver has
> not answered yet, and the round is held back and repeated (loudly, every period) until it does. This
> is what `--no-fast-failed` turns off: with it, the empty first result is published and reported like
> any other. Neither setting ends the process — a node that never answers keeps detecting, it does not
> restart.

### The network interface inventory

`Devices.spec.interfaces` records every network interface on the node, and the RDMA link state that
decides whether the node carries `rdma.capable`. It is its own subsystem (a NIC belongs to the
machine rather than to a manufacturer's accelerators) and it has its own page:
[Network Topology](/gpustack-operator/main/docs/modules/rdma/network-topology/index.md).

### Ascend: two DCMI API generations

Ascend drivers serve one of two mutually exclusive DCMI APIs. V1 is what every driver up to and
including 910B/310P serves; V2 is what the A5/950 generation serves. The Device Manager tries V1 and
falls back to V2, and one allocation-time decision reads which generation answered.

On a V2 host the ledger simply lacks five detector readings that have no V2 counterpart: driver
version, PCIe topology distance, RoCE IP and gateway, the detailed memory info (memory comes from
the HBM query alone), and the multi-die injection policy. Each is optional, so their absence drops
nothing else.

The sixth absence is not optional: V2 declares no container-share query at all, so there is nothing
to read, nothing to write and no `npu-smi` command to offer, and the allocation gate that depends on
it behaves as described [below](#the-device-plugin-allocator).

A 950 reports a chip name carrying an open-ended suffix (`Ascend950PR` and `Ascend950DT` ship
today), so the detector folds every `950*` name onto one soc name, and therefore one family, exactly
as every vendor reader of that name does. A device whose identity die cannot be read is dropped
rather than identified by its PCI address, which repeats on every node and would make two nodes'
accelerators collide on identity.

## The device-plugin allocator

Alongside detection the Device Manager runs the **device-plugin allocator**: it registers per-mode
resources (exclusive / shared / sliced / partitioned) with the kubelet and, on allocation, returns
the container injection and records the allocation into the `Devices` ledger.

An accelerator serves only the family its reported capability can back. An unpartitioned one
advertises the exclusive, shared and logical-sliced token pools; one in a hardware partitioning mode
advertises only the `.partitioned` pool. A family's tokens are absent from the other population, so
the kubelet cannot hand a partition request an accelerator that cannot host it.

### Exclusive and shared

For most manufacturers the injection is the device-visibility env (`NVIDIA_VISIBLE_DEVICES` /
`ASCEND_VISIBLE_DEVICES` / …), which their container runtime turns into device nodes. AMD, Cambricon,
MetaX and Hygon inject the nodes themselves, so a node of theirs needs the vendor driver alone.

A shared grant of N is N [distinct accelerators on one node](/gpustack-operator/main/docs/modules/devices/requests/index.md#the-resource-keys):
the allocation hint offers one token per accelerator, and the ledger charges one share per accelerator.
`Allocate` refuses a grant the kubelet put two tokens of on one accelerator (`FailedPrecondition`), rather
than hand the container fewer accelerators than it asked for. The response is the same visibility
injection as an exclusive grant.

AMD injects `/dev/kfd` plus each granted accelerator's `/dev/dri/card<N>` and `/dev/dri/renderD<N>`,
every one of them required, and sets `AMD_VISIBLE_DEVICES=none`: the variable and the injected nodes
union rather than reconcile, so leaving it live would be a second grant channel.

Cambricon injects what its vendor plugin injects by default: the card's own node, the optional
per-card nodes the host exposes, and the node-level control nodes once per response. Only the card's
own node is required — a card the host exposes none for fails the allocation rather than starting a
container with no accelerator. `CAMBRICON_VISIBLE_DEVICES` is still set, for a deployment that does
run the vendor runtime.

A manufacturer that publishes CDI specifications can carry the grant through CDI. The allocation
response carries the values below; the injection channel determines which component applies them:

| Channel | Response payload | Injected by |
|---|---|---|
| `envvar` (default) | `NVIDIA_VISIBLE_DEVICES=GPU-…` | the vendor's container runtime, *if* it is in the Pod's path. Under a generic OCI handler the variable is inert: the container starts with no accelerator and no error |
| `cdi-annotations` | the annotation `cdi.k8s.io/gpustack-<manufacturer>: <kind>=<id>` | the container engine itself, resolving that name against the specifications already on the node and injecting the device nodes *and* the driver libraries — no vendor runtime in the Pod's path |

The channel is chosen per manufacturer through
[`GPUSTACK_${MANUFACTURER}_DEVICE_INJECTION_STRATEGY`](/gpustack-operator/main/docs/reference/settings/index.md#per-manufacturer-overrides).
Its third value, `auto`, answers per container and falls back to the default `envvar` at the first
of these that holds, logging which one:

1. the Pod names a `runtimeClassName`, whose handler owns injection and whose configuration this
   cannot read;
2. the container engine's configuration could not be read;
3. the engine's default runtime handler is a vendor runtime — the variable already works there, and
   a CDI request would be a second injection path for one container;
4. the engine does not resolve CDI requests;
5. the loaded specifications do not name every granted accelerator. A request naming one they do not
   carry fails the whole container, so a partial match is no better than none.

Only when none holds — the configuration readable, CDI resolvable, every granted accelerator named —
does it request CDI.

The two halves stay distinct: the manufacturer's generator writes what a CDI name means (device
nodes, driver libraries, hooks) into `/etc/cdi` and `/var/run/cdi` on the node, and this operator
writes only the name of what one container was granted, onto that container. Those directories are
read to check the name is there before requesting it, and never written to.

The `.partitioned` family never takes that channel: the instance is materialized at allocation time,
so no pre-generated specification names it.

### Partitioned

The allocator materializes the requested hardware instance (NVIDIA MIG, or the MIG-named partitioning
T-Head and Hygon each ship) on an accelerator it selects itself, and injects only that instance. How
differs by vendor: device nodes for T-Head, which has no container-runtime hook, and the instance's
own registry file bind-mounted at its host path for Hygon, whose runtime scans that directory by
absolute path.

See [Accelerator Requests](/gpustack-operator/main/docs/modules/devices/requests/index.md), [NVIDIA MIG](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md),
[T-Head MIG Operations](/gpustack-operator/main/docs/modules/devices/thead-mig/index.md) and [Hygon MIG
Operations](hygon-mig.md).

### Sliced (logical slicing)

The allocator also applies runtime isolation with decoupled compute and memory budgets: compute
(SM / aicore) from `.sliced.cores-percentage` (default 100 %), VRAM from the per-accelerator memory
request (`.sliced.memory-percentage` preferred over `.sliced.memory-mib`, floored and capped at the
accelerator VRAM), so a slice can cap SM independently of VRAM. Enforcement differs by manufacturer,
see [Logical slicing per manufacturer](#logical-slicing-per-manufacturer).

It also quiets each preload library's verbose per-call logging, so a normal run is not buried in
interception noise. Each variable is injected only if the workload does not set it:

| Variable | Library | Injected value |
|---|---|---|
| `LIBCUDA_LOG_LEVEL` | HAMi-core (NVIDIA, Iluvatar) | `0` |
| `ENPU_LOG_LEVEL` | vcann-rt (Ascend) | `1` |
| `LIBHGGC_LOG_LEVEL` | the T-Head shim | `1` |
| `LIBVROCM_LOG_LEVEL` | the AMD shim | `1` |

Ascend takes one more under the same never-overwrite rule, `ENPU_DSMI_HOOK=1`. It turns on a hook in
the vendored Ascend slicing runtime, so the container's `npu-smi info` reports its HBM quota and the
slice's usage instead of the whole accelerator — the mixed view NVIDIA gives, where `nvidia-smi`
shows the virtual VRAM total while power and temperature stay accelerator-wide.

NVIDIA takes one more under the same rule, `CUDA_DEVICE_ORDER=PCI_BUS_ID`. The memory-limit table is
filled in one enumeration order and read back by CUDA ordinal, and the two coincide only under
`PCI_BUS_ID` (CUDA's default orders by a performance heuristic). The same invariant governs any
integer a workload derives from an NVML index and hands to CUDA, `CUDA_VISIBLE_DEVICES` included.

> **NVML is unaffected by it** — NVML always enumerates by PCI bus id, so the `Index` the Device
> Manager reports matches `nvidia-smi` whether or not the variable is set.

> **Never-overwrite reads the container's own `env:` entries** — an `envFrom:`-sourced value is
> invisible to the allocator, so opting out that way needs an explicit `env:`.

### Positional injection order

A key addressed by position — `CUDA_DEVICE_MEMORY_LIMIT_<i>`, `HGGC_DEVICE_MEMORY_LIMIT_<i>`,
`VROCM_DEVICE_MEMORY_LIMIT_<i>`, `HSA_CU_MASK`'s `GPU_list` — is read against the numbering the
container itself uses, so every allocator emits its entries in one order: **ascending accelerator
index**, the enumeration the detector recorded.

Which vendors that order matters to differs, because it depends on who decides the container's
numbering:

| Vendor | Numbering basis | Order impact |
|---|---|---|
| NVIDIA | NVML/CUDA re-enumerating the visible cards by PCI bus id | **load-bearing** — the emission must match it |
| T-Head | the SDK renumbering the injected nodes by ascending card ordinal (measured) | **load-bearing** |
| AMD | `ROCR_VISIBLE_DEVICES`, which the injection itself states | self-consistent under any one order |
| Hygon | a `device_id` the operator itself writes into each `vdev<i>.conf` — meaning not yet established on hardware | positional, but **persisted**; see below |
| Ascend, Cambricon, MetaX, MThreads | not by position at all — the number travels as a value, or the request is single-accelerator | immaterial |

Where a number does travel as a value, it is the driver's index (Ascend's physical id, Cambricon's
enumeration position), not the operator's logical one. The two coincide only while every accelerator
on the host was detected, so one failing a probe leaves every later accelerator carrying a logical
index below its driver index.

On Ascend the rule is **measured** on a 910B2 (V1 driver), driven on the host: all three sites —
`ASCEND_VISIBLE_DEVICES`, the `/dev/davinci<N>` the vendor runtime then mounts, and a slice's
`npu_info.config` `physical-npu-id` — carry the driver index.

Through a device-manager DaemonSet
under a real kubelet, the measurement covers the whole-accelerator path only; the sliced site was
not re-measured in-cluster.

On a [V2 host](#ascend-two-dcmi-api-generations) the physical id comes
from a different entry point and the rule is **unmeasured** there. The container itself renumbers its
accelerators by ascending physical id, and an out-of-range value fails the container start outright,
which is why naming the wrong accelerator is a silent misplacement.

Hygon is the one whose position outlives the allocation: its figures go into `vdev<i>.conf` files on
the host, and a slot is reused only when the file at that path already names the same accelerator. A
sliced request is admitted for exactly one accelerator today, so the path is always `vdev0.conf` and
the mapping cannot move; if that gate is ever lifted, the order becomes a migration concern rather
than only a correctness one, because the files predate the allocation being retried.

### Preflight checks

`device-manager preflight` reads, on a bare host, the allocation-time preconditions the allocator
reads when a workload lands. It drives each manufacturer's own responder with a synthetic
allocation request rather than a copy of it, so a preflight answer and the allocation it predicts
cannot disagree. The runbook is [Preflight Operations](/gpustack-operator/main/docs/modules/devices/preflight/index.md).

It asks three questions per manufacturer, in order, and each is answerable on its own:

1. **are the devices detected** — the detect pass, cross-checked against the host's own vendor CLI
   where one is established (NVIDIA, Ascend and AMD); for manufacturers without a host reader the detect pass answers alone,
   and a count of zero is the container's own view rather than the host's;
2. **can they be sliced** — the driver read, then a container that is granted a slice and reports
   back the quota rather than the whole accelerator;
3. **can they be managed while sliced** — two named cases: *sidecar visibility*, where the owner's
   and the `sshd` sidecar's allocations are driven in the order the kubelet makes them and the second
   must name nothing the first was not granted (see [SSH-enabled Instances and the visibility
   resource](#ssh-enabled-instances-and-the-visibility-resource)); and *co-tenancy*, two independent
   slices on one accelerator each seeing its own quota.

Every answer is one of three states, exhaustive and mutually exclusive, each with a different
consequence for the allocation it guards:

| State | Meaning | Allocation effect |
|---|---|---|
| `ok` | the capability works, at the depth the row states | proceeds |
| `unavailable` | it is offered and this pass did not establish it | is refused |
| `not-declared` | the accelerator does not offer it, so there is nothing to check | proceeds without it |

**The state says nothing about a driver**, and reading it as one misdiagnoses both directions: a row
is `ok`/`measured` on a manufacturer whose allocator reads no driver at all, and `unavailable` when a
probe ran against a healthy driver and observed no quota. A driver refusing to answer produces an
`unavailable` row too, but that is one case of the state rather than its meaning.

**Nor is `unavailable` a claim that the capability is broken.** A container that could not be got far
enough to show anything lands there beside one that showed the capability failing: a probe image
whose client cannot start reads as `unavailable`, because a pass that waived what it could not
observe would let a node through on an assumption. The row's `reason` is what separates the two.

The state also carries the depth it was reached at, so an assumption is never read as evidence:

| Depth | Action | Establishes |
|---|---|---|
| `declared` | the driver was asked and answered | what the host claims |
| `simulated` | the allocator's own code produced the artifact and it was asserted on, while nothing on the hardware changed | what the allocation would emit |
| `measured` | something ran and was observed | the behavior itself |

Nothing carries a deeper label than it earned. A case that could not be taken to the measured depth
is reported at the depth it reached, with the reason it went no deeper, never as a failure and never
as a pass.

Two of Q3's answers stop short by construction rather than by environment. Sidecar visibility is
answered at `simulated`: a measured one needs the owner's container still running, and every
container this starts is one-shot. A partition-backed accelerator is answered at `declared`:
reaching its visibility response means driving the capability that also creates a partition.

It reaches the host by entering a bind-mounted host root with `chroot`, which gives it the host's own
container CLI and the host's own vendor CLI, which answers with no `/dev` mount in the container at
all. That is what separates *this machine has no accelerators* from *this machine has eight and your
container cannot see them*. Preflight's own code never runs in host context.

> **Why it is not part of `detect`** — `detect` is a pure read, while preflight starts containers
> that hold an accelerator and asks a driver to toggle a mode and put it back. On a node carrying
> live workloads that difference is worth a command of its own. The two cannot drift, because
> preflight reuses the detect pass rather than reimplementing it.

## Logical slicing per manufacturer

Every sliceable manufacturer has real per-slice runtime isolation, but only NVIDIA, Iluvatar,
Ascend and T-Head take both budgets from a preload library. Every preload library is activated
through `/etc/ld.so.preload`.

| Manufacturer | Enforcer | Quotas and injection |
|---|---|---|
| NVIDIA, Iluvatar | HAMi-core `libvgpu.so` | `CUDA_DEVICE_SM_LIMIT` / `CUDA_DEVICE_MEMORY_LIMIT_*` |
| Ascend | vcann-rt `libvruntime.so` | an `npu_info.config` carrying `aicore-quota` / `memory-quota` |
| T-Head | the pair `hggc_quota.so` (enforcement) + `hgml_dlsym_hook.so` (visibility) | `HGGC_DEVICE_SM_LIMIT` / `HGGC_DEVICE_MEMORY_LIMIT_*` |
| AMD | `libvrocm.so` for VRAM, plus a hardware **compute-unit mask** the operator derives and ROCr enforces | `VROCM_DEVICE_MEMORY_LIMIT_<i>` in bare MiB + `VROCM_LEDGER_PATH`, and `HSA_CU_MASK` |
| MThreads | the host sGPU kmod | `MTHREADS_QOS_*` env vars — the compute share is a scheduling weight, not a hard cap |
| Hygon | the host DTK/hyhal runtime | a per-pod `vdev.conf` (a `cores%`-derived CU bitmask + VRAM cap) mounted read-only at `/etc/vdev/docker/` |
| MetaX | a sysfs `sgpu` subdevice | the accelerator is put in `sgpu` mode, a `cores%`-derived compute quota + VRAM cap written under a `fixed-share` scheduling class to `/sys/bus/pci/devices/<BDF>/sgpu/create`, then `METAX_SGPUS` plus the accelerator device nodes injected for the host MetaX runtime |
| Cambricon | a cnDev sMLU profile + instance | a profile with `mluQuota = cores%` and `memorySize` set to the VRAM budget is created or reused, a subdevice instantiated, its device nodes `/dev/cambricon_dev*` / `/dev/cambricon_ipcm*` / the instance node injected alongside the node-level control nodes, with a `VIRTUAL_DEVICES` env fallback for `--use-runtime` deployments since sMLU does not support CDI |

Iluvatar reuses HAMi-core, corex being CUDA-compatible. It keeps the accelerator visible through
`IX_VISIBLE_DEVICES` and needs `ix-container-runtime` to inject corex, so a sliced Iluvatar Pod must
carry `runtimeClassName: iluvatar` — without it the preloaded `libvgpu.so` finds no corex
`libcuda.so.1` to hook. HAMi-core-on-corex is verified at symbol level against a real corex driver
but **not on Iluvatar hardware**: the pairing is advertised and injected, and unvalidated in hardware.

Ascend enables container-share mode for `sliced`, `shared` and `visibility` allocations, which
can place a second tenant on an accelerator. Exclusive allocation leaves that flag alone.
If the flag cannot be set, allocation fails with the accelerator and flag in the diagnostic.

`npu-smi` warns that enabling sharing carries security risks and recommends one user per chip.
On a measured 910B2, an exclusive container saw full VRAM and opened the device identically in
both flag states. Slice isolation comes from GPUStack's [allocation
ledger](#container-identification-and-cross-mode-exclusion) and vcann-rt's `memory-quota` limit.

A missing container-share entry point refuses allocation; no `npu-smi` command can add that API.
For the [V2 generation](#ascend-two-dcmi-api-generations), which declares no such flag, the
allocator allows all three tenanted modes and logs the accelerator. Whether V2 enforces an
equivalent guard by another mechanism remains unmeasured.

Other read failures still lead to an attempt to enable the flag. If that write also fails, the
error reports both failures and the remedy.

The flag persists in the driver, so an accelerator that has hosted a tenant stays shareable until the
host reboots or an operator clears it with `npu-smi set -t device-share`.

Cambricon logical slicing requires sMLU mode. The allocator enables it for `sliced` allocations
and leaves it enabled until an administrator clears it with `cnmon`. Turning it off while other
Pods have slices would strand those Pods.

If the library or driver has no sMLU API, allocation fails without suggesting a command. Other
write failures report the PCI address and cnDev index and suggest `cnmon set -c <index> -smlu on`.
Confirm the ordinal with `cnmon` before using it: equality with the reported index is unverified.

Cambricon slice capacity is advertised even when sMLU mode is off, so allocation can enable it.
It needs no slicing library from GPUStack's image. Advertised capacity alone does not establish
that the driver can serve a slice; the mode and profile APIs are checked during allocation.

The detector cannot read the `cntoolkit` userspace version from its container. The effect of
sMLU mode on a whole-card tenant remains unmeasured; the Ascend observation above does not
establish that behavior for Cambricon.

AMD splits the two dimensions across two enforcers, alone among the manufacturers. Memory is a
preload library like the others, accounting in a per-container region named by `VROCM_LEDGER_PATH`.
Compute is no variable at all: ROCm enforces it in hardware through `HSA_CU_MASK`, which ROCr reads
while initialising, before any preloaded code exists.

So the operator derives the mask from the accelerator's topology and injects it; the library never
sees it.

A sliced AMD container gets its device nodes the same way the exclusive and shared paths do: the
allocator injects `/dev/kfd` plus each granted accelerator's `/dev/dri/card<N>` and
`/dev/dri/renderD<N>` itself. `AMD_VISIBLE_DEVICES` carries the literal string `none` — an explicit
instruction to any `amd-container-runtime` on the node to add nothing.

The variable and the injected nodes union rather than reconcile: measured, a node with the runtime
installed and the variable naming an accelerator the injected nodes do not gives the container both.

`ROCR_VISIBLE_DEVICES`, read by the ROCm user-space runtime to filter and order its agents, keeps
carrying the granted accelerators' `GPU-<hex>` UUIDs, and must name exactly the accelerators whose
nodes were injected: an entry ROCr cannot resolve to a visible agent does not drop that entry, it
yields **zero GPU agents**, measured and silent.

That order is also the index space of the other two variables: `HSA_CU_MASK`'s `GPU_list` index and
`VROCM_DEVICE_MEMORY_LIMIT_<i>`'s `<i>` are positions in the `ROCR_VISIBLE_DEVICES` list, never
physical ordinals. The three are emitted together and must stay in step.

> **Why this one needs a probe** — a CU mask fails **open**: one ROCr rejects yields no error, no log
> line, no changed return code, and the container gets the whole accelerator, while `rocm-smi` and
> `amd-smi` read sysfs and never see a mask. So the allocator mounts two tools beside the library:
>
> - `rocm-cumask-check` runs a kernel, reads the physical units its own waves landed on, and exits `0`
>   only if they are the units the mask asked for;
> - `rocm-monitor` prints the memory quota and what is charged against it.
>
> A slice behaving like a whole accelerator is then one command from diagnosis, on a node nobody
> watches.

Because the mask is quantised to the accelerator's allocation atom, the smallest requestable
percentage is a per-accelerator property: 9 % on a 60 CU / 3 shader-engine part, 3 % on a
304 CU / 8 XCC one. A request below it is refused at allocation time, the message naming that
minimum, rather than rounded up into a ceiling nobody asked for. One above it that misses the atom is
aligned **down**, and the allocator logs the percentage delivered.

> **Admission does not know that minimum yet** — the webhook validates `1`–`100` and nothing
> publishes the per-accelerator floor, so a request below it is admitted, scheduled, then refused by
> the device plugin: the Pod fails to start and keeps failing. Until it is published, watch the very
> small request (on an accelerator with many shader engines, single-digit percentages may not be
> servable at all).

**T-Head emits the compute figure even at 100 %**, because that library refuses an accelerator whose
figure is missing rather than reading absence as "no cap".

Its sliced response carries **no** visible-devices env: like its other modes it passes the
accelerator's device node plus the two shared control nodes, adding only the library mounts, the
quota env and a per-container directory for the ledger region under the pod working directory — per
container, because the region is addressed by container-local accelerator index.

The visibility half makes the container's `ppu-smi` report its quota rather than the physical
accelerator, by interposing `dlsym`. A mounted `ppu-monitor` reads quota and usage for both dimensions
from the container's ledger region (`HGGC_LEDGER_PATH`), the only place the compute cap can be seen
(no `ppu-smi` field carries it).

A workload image bringing its own `dlsym` interposer through `LD_PRELOAD` (processed before
`/etc/ld.so.preload`) leaves that half loaded but never entered: the quota still applies, but
`ppu-smi` shows the whole accelerator. The library cannot detect this, so it is a caveat, not an
error.

### Where the preload libraries come from

The preload libraries are compiled into the operator image per runtime version and staged onto the
host (`/var/lib/gpustack/operator/lib`) by a device-manager **init container**.

The allocator mounts the matching library plus a per-pod working directory into each sliced
container, reclaiming those directories once their pods are gone.

**Iluvatar reuses the NVIDIA CUDA 12 HAMi-core build**: corex exposes a CUDA-compatible
`libcuda.so.1`, so the same library serves, one flat directory, no runtime-version subdivision.

**AMD's library is one artifact for every ROCm version**: `libvrocm.so` links no ROCm object, so
every runtime entry point resolves at load time rather than link time, and its directory is flat
where NVIDIA's and Ascend's carry a subdirectory per runtime generation. It ships the two readers
(`rocm-monitor`, `rocm-cumask-check`) the allocator mounts beside it.

ROCm publishes no `aarch64`
user space, so the **`arm64` operator image carries no AMD shim**, and no AMD node either: the
detector's libraries do not load there.

**T-Head's pair is built from this repository's own sources and carries no runtime-version
subdirectory**, the PPU SDK living in the workload container rather than ours.

That SDK is `x86_64`-only, so the **`arm64` operator image carries no PPU shim** — the detector does
not check for one, on the ground that a PPU only exists in an `x86_64` host.

## SSH-enabled Instances and the visibility resource

For an **SSH-enabled Instance** the workload runs in a two-container Pod: `main` (the user image) and
`sshd` (an Alpine sidecar that `nsenter`s into `main`). The accelerator request and its
runtime-isolation artifacts go on `main`, where the workload runs, and where the SSH shell entering
`main`'s namespaces lands. `sshd` requests an internal-only
`device.gpustack.ai/<manufacturer>.visibility` resource, quantity = `main`'s accelerator count.

The SSH sidecar reuses the devices held by `main`, with the plain environment or device-node
injection described in [Exclusive and shared](#exclusive-and-shared). It receives no slicing
artifacts and consumes no additional accelerator ledger capacity.

The allocator matches the sidecar to `main` through an in-process reservation, then the Pod's
`device.gpustack.ai/accelerator.allocated` annotation. The kubelet allocates `main` first; the
annotation lets the sidecar recover the same grant after a Device Manager restart.

**What that env names follows the owner's family**: the accelerator(s) `main` holds for an
exclusive/shared/sliced owner; for a **partition-backed** owner the partition itself, never the
parent accelerator, which hosts other tenants' partitions too.

The sidecar's allocation is a device-cgroup grant and nothing else wherever the owner's grant travels
in its environment: the SSH session `nsenter`s into `main` and inherits that environment, needing no
injection.

Wherever the grant travels as device nodes it is carried here too, because the same non-sliced
responder serves this mode — AMD, Cambricon, Hygon and MetaX inject their control and DRM nodes for
the sidecar as they do for the owner. NVIDIA follows whichever channel its resolver settled on, so a
CDI request appears here as well.

For a partition-backed owner, the allocator reads the owner's `.partitioned.<kind>-<profile>`
request and verifies that its durable node-local ownership record still names a live partition.
See [NVIDIA MIG](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md#requesting-a-partition), [T-Head
MIG](thead-mig.md#requesting-a-partition) and [Hygon MIG](/gpustack-operator/main/docs/modules/devices/hygon-mig/index.md#requesting-a-partition).

A responder lacking that capability, or unable to substantiate the identity, fails the admission
closed rather than widening the grant back to the accelerator.

The visibility resource is advertised per accelerator as a pool of `SlicedResourceMaxSize` tokens
outside the known-acceleratable families. Because the family classifier does not know it, the resource
never gates scheduling, and admission never reads it as a second accelerator mode. Every accelerator
backend registers it.

The per-accelerator AdmissionCheck ([Admission](/gpustack-operator/main/docs/modules/devices/admission/index.md)) re-checks feasibility only **before**
admission: an admitted Workload's allocation is already in the ledger, so re-evaluating would count a
slice against itself.

## Container identification and cross-mode exclusion

An `Allocate` call names the assigned device IDs but not the pod it serves, and the allocator has to
know both. It asks the kubelet first, over its pod-resources API: the kubelet adds a Pod before
admitting it and admits one at a time, so the container this call serves is the one the kubelet holds
without a device of the resource yet.

That answer outranks the allocator's own matching, because Pod creation timestamps keep whole
seconds only: two slices created together and bound in the other order would be recorded on each
other, each container getting the other Pod's memory limit.

When the kubelet cannot be asked, names
none of the candidates, or names several, the allocator matches the node's pending pods itself —
dropping candidates the call could not serve, skipping one already holding a reservation, taking the
oldest survivor. A retried call is not charged twice.

Once the container is known, a logical slice its accelerator cannot hold is refused with
`FailedPrecondition`: one with no free slot, or without the units the slice needs. The kubelet fails
that Pod with `UnexpectedAdmissionError`. The refusal reads the same room the allocation hint does,
so when it refuses, no candidate fits that accelerator.

Both the kubelet lookup and the refusal have
an off switch, `GPUSTACK_DEVICE_PLUGIN_IDENTIFY_BY_KUBELET` and
`GPUSTACK_DEVICE_PLUGIN_SLICED_ALLOCATE_GATE` (see
[Settings](/gpustack-operator/main/docs/reference/settings/index.md#general-variables)).

Reservations key on **(Pod UID, container)** and the durable
`device.gpustack.ai/accelerator.allocated` annotation is a **map keyed by container name**, so two
containers of one group each holding a live claim are both recorded and both charged. An entry
charges its accelerator until its **Pod** is gone — the reclaimer and the kubelet also scope a device
to the Pod's life, not the container's.

A Pod that has **finished**, in phase `Succeeded` or `Failed`, is the exception: the kubelet has taken
back its exclusive and shared cards and its logical slices, so the ledger stops charging those. A
hardware partition and a MetaX or Cambricon slice stay charged until the Pod is gone, because the
reclaimer destroys them only then. `GPUSTACK_LEDGER_RELEASE_TERMINATED_PODS=false` turns the exception
off ([settings](/gpustack-operator/main/docs/reference/settings/index.md#general-variables)).

One thing takes an entry back earlier, and it is the only one: an allocation the manufacturer
responder refuses **after** the record is written is given back on the spot — the entry and the
reservation both — because the kubelet does not start that container. A claim the container already
held is restored rather than dropped, and a give-back that cannot reach the API is retried until it
lands or the Pod is gone.

The cross-mode invariant is what this layer enforces: an accelerator kubelet assigned that another
mode holds, per the ledger status or the in-process reservation, is refused with
`FailedPrecondition` — an exclusive tenant truly owns its accelerator on every path, Kueue or raw.

`ListAndWatch` keeps tokens advertised while an accelerator is held in another mode, preserving
kubelet's checkpointed allocations, but reports those tokens as Unhealthy. Health follows the
ledger and reservations and is updated on reservation and release. The allocation check remains
the final guard against conflicting modes.

The visibility resource remains allocatable for the SSH sidecar on its workload's accelerator,
whatever mode the workload holds.

It also maps a batch of identical accelerator Pods admitted together (e.g. by Kueue) one-to-one to
distinct Pods, keeping annotations and the ledger correct instead of double-attributing one and
losing another. The `sshd` visibility path re-finds its Pod's **non-self** accelerator allocation,
rather than skipping reservations, so the sidecar still resolves after a Device Manager restart.

## Placement preference

For the accelerator-bound families, tokens name an accelerator, so the kubelet's pick of a token is
the pick of an accelerator; the plugin only orders the candidates it offers back.

For a logical slice it offers the tokens of the most-occupied accelerator that still fits: one
already serving slices beats a pristine one, ties broken by the accelerator's position within its
group, so identical requests against identical state place identically. Slices coalesce instead of
each opening a fresh accelerator and stranding a node whose every accelerator is partly used but none
can host one large claim.

It stays a preference: the kubelet may take another accelerator. For a logical slice allocation
itself is the backstop — it refuses one short of a slot or of room, reading the same room the hint
does (see [Container identification](#container-identification-and-cross-mode-exclusion)). The call
is advisory by API contract, so under a restrictive TopologyManager policy the kubelet allocates the
NUMA-aligned set before consulting the plugin at all.

### The partitioned family: fungible tokens

The `Partitioned` family is the one exception to accelerator-bound tokens. Its allocation treats the
kubelet's device IDs as a *quantity* and chooses the accelerator itself, against the live geometry.

Accelerators are **packed, not spread**: the most-occupied one that still fits wins, keeping a
sibling whole for a later whole-accelerator profile. A retried allocation for a container that
already has an allocation reuses the accelerator it used, read from the reservation and then from the
durable annotation.

Because no partition token names an accelerator, that family's health is a pure node-level count of
remaining room — `allocated + remaining` published over a stable set of IDs, never removing an ID a
live allocation holds — and it reports **no** NUMA topology, since the kubelet would otherwise align
CPU and memory to an accelerator the plugin may not use.

One residual remains: a partition an administrator carves out of band is invisible to every
annotation-derived key, so hand-carving on a managed node is unsupported (see
[Accelerator Requests](/gpustack-operator/main/docs/modules/devices/requests/index.md#limitations)).

## One driver stack per node

A node hosts a single driver/runtime stack per manufacturer, so every accelerator group of a given
manufacturer on it shares one driver and runtime version. The per-runtime-version library
subdirectory (`cuda-<major>` / `cann-<major>-<family>`) the allocator picks from the first allocated
accelerator is therefore correct for every accelerator in a sliced allocation.

**Nothing below the detector re-checks that such a subdirectory was actually built.** So on Ascend
the detector offers logical slicing only for the family/runtime-major pairs the image ships a
vcann-rt for. A pair with no build stage is not advertised at all, instead of being advertised and
then failing to start the container on a missing directory. Adding a build stage widens that set.

The allocator still guards against a mismatch **defensively** (NVIDIA rejects a sliced allocation
spanning different CUDA majors; Ascend rejects a multi-accelerator sliced allocation, since
vcann-rt's `npu_info.config` models a single physical NPU), so any future regression fails the
allocation loudly instead of silently mounting an incompatible library.

---

**See also** — [Accelerator Requests](/gpustack-operator/main/docs/modules/devices/requests/index.md) (the resource keys these families
serve) · [NVIDIA MIG Operations](/gpustack-operator/main/docs/modules/devices/nvidia-mig/index.md) · [T-Head MIG
Operations](thead-mig.md) · [Hygon MIG Operations](/gpustack-operator/main/docs/modules/devices/hygon-mig/index.md) ·
[Settings](/gpustack-operator/main/docs/reference/settings/index.md)

**Next** → [Scheduling Chain](/gpustack-operator/main/docs/modules/devices/scheduling/index.md) — how these labels and the ledger become Kueue
objects.
