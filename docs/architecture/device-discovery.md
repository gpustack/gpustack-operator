# Device Discovery

> **Purpose** — how a node's hardware becomes labels and a per-accelerator ledger: what NFD
> publishes, what the Device Manager detects, and what the device-plugin allocator does at `Allocate`.
> **Audience** contributors · **Prerequisites** [Architecture](../architecture.md) · **Read time** ~20 min

Stages 1 and 2 of the four-stage chain. Stages 3 and 4 are in [Scheduling
Chain](scheduling-chain.md).

## Contents

- [Device, Accelerator, Resource](#device-accelerator-resource)
- [Stage 1: Node Feature Discovery (NFD)](#stage-1-node-feature-discovery-nfd)
- [Stage 2: the Device Manager (DM)](#stage-2-the-device-manager-dm)
- [The device-plugin allocator](#the-device-plugin-allocator)
- [Logical slicing per manufacturer](#logical-slicing-per-manufacturer)
- [SSH-enabled Instances and the visibility resource](#ssh-enabled-instances-and-the-visibility-resource)
- [Container identification and cross-mode exclusion](#container-identification-and-cross-mode-exclusion)
- [Placement is a preference, not a decision](#placement-is-a-preference-not-a-decision)
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
  ordinal (see [T-Head MIG Operations](../operation/thead-mig.md)).
- **manufacturer** — the company. Its native code is *the manufacturer's library, SDK or binding*.

The `pkg/device` package doc carries the same diagram.

## Stage 1: Node Feature Discovery (NFD)

NFD comes from the `node-feature-discovery` subchart, or from the cluster itself (see [Installation Modes](installation-modes.md)). It performs three jobs.

### Job 1 — PCI vendor labels

Labels every Node carrying a PCI display/accelerator-class device with:

```
feature.node.kubernetes.io/pci-${PCI_VENDOR_ID}.present: "true"
```

An NVIDIA device gives `feature.node.kubernetes.io/pci-10de.present: "true"`. Classes `02`, `03`,
`0b` and `12`, from the chart's
`node-feature-discovery.worker.config.sources.pci.deviceClassWhitelist`; the rule below matches the
same classes from `pkg/nodefeature`, and the DM's sysfs scan reads `GPUSTACK_PCI_CLASS_PREFIXES` (see
[Settings](../settings.md)).

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

**The general(CPU) node key.** The Worker normalizes these into the node's general(CPU) node key
(`nodefeature.ExtractGeneralNodeKey`), never empty and always carrying the node's real CPU identity,
as `${cpuManufacturer}-${id}`:

- `cpuManufacturer` — the lowercased `cpu-model.vendor_id` label, a
  [cpuid](https://github.com/klauspost/cpuid) vendor enum name (`Intel` → `intel`, `AMD` → `amd`);
  `generic` when unknown or unreported.
- `id` — the sanitized `feature.gpustack.ai/cpu-name` annotation: trademark markers and the trailing
  `" CPU @ …"` frequency part dropped, a leading manufacturer prefix deduplicated, truncated to the
  naming budget (e.g. `amd-epyc-7763`); unreported, the cpu-model family and id labels instead (e.g.
  `amd-25-1`).
- The key degrades to `generic` only when NFD reports no CPU identity at all.

Whether that manufacturer **subdivides** the pools is a separate runtime decision, taken at the
aggregation layer by
[`instance-type-aware-cpu-manufacturer`](../settings.md#online-adjustable-settings) (see [Scheduling
Chain](scheduling-chain.md#naming-and-grouping)); the key stays CPU-accurate so the finest-grained
`ResourceFlavor`s regroup without rewriting.

**The key deliberately does not encode os/arch.** os/arch is appended in full to every ResourceFlavor
/ ClusterQueue / InstanceType **name** (`…-linux-arm64`, never abbreviated) and pinned on the
ResourceFlavor's `spec.nodeLabels` (`kubernetes.io/os`, `kubernetes.io/arch`).

> **Why** — cpu-model family/id are independent numbering spaces on x86 (CPUID) and arm64 (MIDR), so
> `25-1` can legitimately appear on both, and amd64/arm64 binaries are not interchangeable: their
> capacity must never pool into one flavor/queue. It also reclaims the label-length budget the old
> abbreviated `-ln-x64` suffix consumed.

### Job 3 — the non-accelerated marker

Labels Nodes with **no** accelerator device from any known manufacturer with:

```
feature.gpustack.ai/acceleratable: "false"
```

It contrasts explicitly with the `acceleratable: "true"` the Device Manager reports later, which also
corrects false negatives.

### The `gpustack-cpu-info` NodeFeatureRule

Jobs 2 and 3 are the work of the `gpustack-cpu-info` **NodeFeatureRule**, applied by the worker at
startup in every install mode (see [The chart deploys workloads, the worker applies the custom
resources](installation-modes.md#the-chart-deploys-workloads-the-worker-applies-the-custom-resources)).
Its two matcher lists come from facts that exist for other reasons:

- the PCI vendor IDs of the manufacturers the worker manages — `--manufacturer`, filled by the chart
  from `global.manufacturers`, the same map that drives the device-manager DaemonSets, so a
  manufacturer added there is labelled, detected and given a device manager in one edit;
- the PCI classes `pkg/nodefeature` calls acceleratable, which are the classes NFD publishes.

Two Go tests hold the chart's `global.manufacturers` map and `deviceClassWhitelist` equal to
`pkg/nodefeature`.

**The manufacturer map is where a manufacturer's whole identity lives** — one row each:

| Field | What it carries |
|---|---|
| `pciVendorID` | the PCI vendor ID Job 1 labels the node with |
| `resourceName` | the name this manufacturer's device-plugin advertises |
| `runtimeName` | the runtime this manufacturer's workloads run under |
| `partitionKind` | this manufacturer's hardware partitioning kind |
| `runtimeInjectsDriver` | the user-space driver reaches a container only through the runtime |
| `runtimeInjectsDevices` | the allocator contributes no device node, so only the runtime turns an allocation into `/dev` entries |

The two `runtimeInjects*` fields say what a container misses without that manufacturer's container
runtime, and answer for different consumers:

- `runtimeInjectsDriver` is why the NVIDIA and MThreads **device-managers** run under a RuntimeClass,
  while every other manufacturer's reads its management library from a hostPath mount.
- `runtimeInjectsDevices` is the **workload's** need alone, for a manufacturer whose allocator
  contributes no device node of its own — Ascend and Iluvatar, which emit `ASCEND_VISIBLE_DEVICES`
  and `IX_VISIBLE_DEVICES` and nothing else. AMD used to be a third and no longer is: its allocator
  injects `/dev/kfd` and the accelerator's DRM nodes itself.

**Either** — never `runtimeName` — decides which RuntimeClasses the chart creates
(`deviceManager.createRuntimeClasses`, and only where the class is absent or already this release's),
so that set is narrower than the manufacturers merely stating a `runtimeName`.

> **Why not `runtimeName`** — it is the class the operator will *use*, while either `runtimeInjects*`
> fact lets the runtime's presence be **inferred**: a manufacturer missing what it names cannot work
> unless the handler is registered.
>
> A manufacturer with neither injects its own device nodes and needs no RuntimeClass — creating one
> would break it, since the operator attaches a RuntimeClass whenever one exists and the kubelet
> rejects a Pod naming an unconfigured runtime. A class the manufacturer's own operator created is
> still used; the chart never conjures one.

## Stage 2: the Device Manager (DM)

For each known manufacturer a DaemonSet `gpustack-operator-device-manager-${manufacturer}` is created
with a node selector on the NFD PCI label: a node labeled
`feature.node.kubernetes.io/pci-10de.present: "true"` gets a Pod from
`gpustack-operator-device-manager-nvidia`.

The Helm chart normally renders them (`deviceManager.enabled=true`, the default). With
`deviceManager.enabled=false` it renders none and does not hand the install back to the worker: a
worker deployed by this chart never installs applications at runtime. The worker installs them only
where no chart deploys it, see [Installation Modes](installation-modes.md).

### Detection and the accelerator feature labels

The DM detect loop (`pkg/devicemanager/detector/detector.go`) periodically detects accelerators and
reports a NodeFeature `${NODE_NAME}-gpustack-device-manager` (owned by the Node), its labels built by
`nodefeature.ConstructAcceleratableNodeLabels`. Each model is keyed by the accelerated device key
`${aKey} = ${manufacturer}-${id}`, `id` being the product name sanitized to Kubernetes label naming
rules:

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
`acceleratable.feature.gpustack.ai/` key namespace. `manufacturer` is one supported by
`pkg/devicemanager/detector`: NVIDIA, AMD, Ascend, Cambricon, Hygon, Iluvatar, MetaX, MThreads,
T-Head — their PCI vendor IDs, resource names and runtime class names all overridable (see
[Settings](../settings.md#per-manufacturer-overrides)).

### The `Devices` ledger

The NodeFeature is owned by the Node. The DM also reports a **`Devices` custom resource** named after
the node, stamped with the accelerator flavors' selector labels (the feature key +
`kubernetes.io/os|arch`) so the pool's queue can reverse-look-up its Devices.

> **Why owned by the Node** — a `Devices` is cluster-scoped, and the garbage collector resolves a
> cluster-scoped dependent's owner in the empty namespace, so a namespaced owner like the NodeFeature
> is unresolvable and would leave it uncollected. Cluster-scoped to cluster-scoped, it is collected
> with the node it describes.

Its `.status` holds the per-accelerator **`AcceleratorAllocation` ledger**: each accelerator's `mode`
(free / exclusive / shared / sliced / partitioned) and `Remaining` credit budget, plus, for a
hardware-partitioned one, its allocated and still-placeable partition profiles. This is the **single
authoritative accounting** of accelerator occupancy, driving the InstanceType four-view display *and*
feeding the per-accelerator AdmissionCheck (see [Admission](admission.md)).

The DM re-detects whenever the device set or health changes. A separate `NodeDevicesReconciler` syncs
the `gpustack.ai/managed` mark from the Node onto the same-named `Devices`, so the per-node DM never
asserts a node-management decision it does not own.

> **A pass that failed is not a manufacturer with no accelerators** — every detector answers an absent
> driver, a library that will not initialise and a bus holding no card with an empty list and no error.
> So an error means that pass could not measure, and carries no claim about the hardware. The loop
> reports such a manufacturer as it was **last detected**: its allocator keeps serving, the `Devices`
> keeps its group and the node keeps that family's capacity keys. A manufacturer that has never
> answered is still absent, and a pass that ran and found nothing is what undetects one — which is
> also what drops whatever was held for it, so a later failure cannot resurrect a card that was pulled.
>
> The monitor pass cannot be carried forward the same way, because a sample is worth what its
> timestamp claims. A manufacturer it could not measure is simply absent from the sample, and is named
> as unmeasured so that its absence is not read as accelerators that went away — which would take the
> loop round again on no evidence.

> **Before the first detection, a DM asked for one manufacturer reports nothing at all** — no
> `Devices`, no NodeFeature, nothing published to the allocator. Its DaemonSet is scheduled by that
> manufacturer's PCI vendor label, so a node that answers with no accelerators is one whose driver has
> not answered yet, and the round is held back and repeated (loudly, every period) until it does. This
> is what `--no-fast-failed` turns off: with it, the empty first result is published and reported like
> any other. Neither setting ends the process — a node that never answers keeps detecting, it does not
> restart.

### Ascend: two DCMI API generations

**Ascend drivers serve one of two mutually exclusive DCMI APIs, and `binding/dcmi` absorbs the
difference so that no caller enumerates or addresses devices differently.** V1 is what every driver up
to and including 910B/310P serves. V2 (`dcmiv2_*`) is what the A5/950 generation serves. `DCMI.Init`
tries V1 and falls back to V2; `APIVersion()` reports which answered.

One caller does read that accessor: the allocator, for the single decision below that turns on the
generation itself rather than on a reading.

The fallback keys on the driver *refusing* V1 rather than on `dlsym` finding no V1 symbol, because the
absence is not what distinguishes the two: a V2 driver exports every V1 entry point and answers each
with `NOT_SUPPORT`. An entry point that really is missing is treated as another refusal and falls back
just the same. That refusal is also why the queries below need no code to fail — they pass through and
the driver says no.

**V2 has no card level.** It enumerates devices flat, indexed by the number V1 calls the logic id, so
the binding presents each V2 device id as a card holding exactly one device: `cardId == devId ==
logicId`, and `deviceId` is always 0. Any other second coordinate is refused with
`INVALID_DEVICE_ID` rather than resolved to the card, so a stale index cannot be served a whole
accelerator's readings under another device's name.

`PhysicalIndexes` therefore reads `{physical id, device id, 0}` on a V2 host, in the same three-entry
shape the allocator already expects.

**Five detector readings have no V2 counterpart**, so on a V2 host the ledger simply lacks them:
driver version, PCIe topology distance, RoCE IP and gateway, the `memory_info` v2/v3 structs (memory
comes from the HBM query alone), and the multi-die injection policy. The detector already treats each
as optional, so their absence drops nothing else.

**The container-share flag is a sixth absent query, and the one absence that is not merely optional**:
it belongs to the allocator rather than the detector, and it gates admission instead of filling in a
field. That is the decision the generation accessor exists for, and it is described with the rest of
the share preflight [below](#the-device-plugin-allocator).

**The generation is also the one named by prefix.** A 950 reports a chip name carrying an open-ended
suffix — `Ascend950PR` and `Ascend950DT` ship today — so the detector folds every `950*` name onto one
soc name, and therefore one family, exactly as every vendor reader of that name does. Listing the
suffixes instead would leave the next one with no family at all, since no other fallback matches a
name starting with 950.

> **Why** — the uuid comes from a die read, and A5 uses a die type the public V2 header does not
> enumerate: `DDIE`, which the vendor names as that chip's uuid. The V2 die query asks for the virtual
> die and then `DDIE`. A device whose die cannot be read is **dropped**, never identified by its PCI
> address — `Accelerator.ID` is universally unique by contract while a BDF repeats on every node, so
> substituting it would make two nodes' accelerators collide on identity.

## The device-plugin allocator

Alongside detection the DM runs the **device-plugin allocator** (`pkg/devicemanager/allocator/<mfr>`,
on `pkg/deviceplugin`): it registers per-mode resources (exclusive / shared / sliced / partitioned)
with the kubelet and, on `Allocate`, returns the container injection and records the allocation into
the `Devices` ledger.

**An accelerator serves only the family its reported capability can back.** An unpartitioned one
advertises the exclusive, shared and logical-sliced token pools; one in a hardware partitioning mode
advertises only the `.partitioned` pool. A family's tokens are absent from the other population, so
the kubelet cannot hand a partition request an accelerator that cannot host it.

### Exclusive and shared

For most manufacturers the injection is the device-visibility env (`NVIDIA_VISIBLE_DEVICES` /
`ASCEND_VISIBLE_DEVICES` / …), which their container runtime turns into device nodes. AMD, Cambricon,
MetaX and Hygon inject the nodes themselves, so a node of theirs needs the vendor driver alone.

AMD injects `/dev/kfd` plus each granted accelerator's `/dev/dri/card<N>` and `/dev/dri/renderD<N>`,
every one of them required, and sets `AMD_VISIBLE_DEVICES=none`: the variable and the injected nodes
union rather than reconcile, so leaving it live would be a second grant channel.

Cambricon injects what its vendor plugin injects by default: the card's own node, the optional
per-card nodes the host exposes, and the node-level control nodes once per response. Only the card's
own node is required — a card the host exposes none for fails the allocation rather than starting a
container with no accelerator. `CAMBRICON_VISIBLE_DEVICES` is still set, for a deployment that does
run the vendor runtime.

A manufacturer that publishes CDI specifications can carry the grant that way instead. The two
channels put different things in the same allocate response, and differ in who performs the injection:

| Channel | What the response carries | Who injects |
|---|---|---|
| `envvar` (default) | `NVIDIA_VISIBLE_DEVICES=GPU-…` | the vendor's container runtime, *if* it is in the Pod's path. Under a generic OCI handler the variable is inert: the container starts with no accelerator and no error |
| `cdi-annotations` | the annotation `cdi.k8s.io/gpustack-<manufacturer>: <kind>=<id>` | the container engine itself, resolving that name against the specifications already on the node and injecting the device nodes *and* the driver libraries — no vendor runtime in the Pod's path |

The channel is chosen per manufacturer through
[`GPUSTACK_${MANUFACTURER}_DEVICE_INJECTION_STRATEGY`](../settings.md#per-manufacturer-overrides).
Its third value, `auto`, answers per container and only ever moves off the default on a positive
fact — it keeps `envvar` at the first of these that holds, and logs which one:

1. the Pod names a `runtimeClassName`, whose handler owns injection and whose configuration this
   cannot read;
2. the container engine's configuration could not be read;
3. the engine's default runtime handler is a vendor runtime — the variable already works there, and a
   CDI request would be a second injection path for one container;
4. the engine does not resolve CDI requests;
5. the loaded specifications do not name every granted accelerator. A request naming one they do not
   carry fails the whole container, so a partial match is no better than none.

The division of labour is worth stating plainly, because the two halves are easy to conflate. The
manufacturer's generator writes what a name **means** — device nodes, driver libraries, hooks — into
`/etc/cdi` and `/var/run/cdi` on the node. This operator writes only the **name** of what one container
was granted, onto that container.

Those directories are read to check the name is there before requesting it, and written to never: two
writers on a node's description of the same hardware is a race whose loser is whichever the engine
loaded second.

The `.partitioned` family never takes that channel: the instance below is materialized at allocation
time, so no pre-generated specification names it.

### Partitioned

It materializes the requested hardware instance (NVIDIA MIG, or T-Head's own MIG-named partitioning)
on an accelerator it selects itself, and injects only that instance — as device nodes rather than an
environment variable for T-Head, which has no container-runtime hook; see [Accelerator
Requests](../accelerator-requests.md), [NVIDIA MIG](../operation/nvidia-mig.md) and [T-Head MIG
Operations](../operation/thead-mig.md).

### Sliced (logical slicing)

It additionally applies **runtime isolation** with **decoupled compute and memory budgets**: compute
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

> **Why the last three are `1`, not `0`** — their levels are not HAMi-core's: `1` logs a line per
> *denial*, not per intercepted call, so `0` would hide the diagnostics of a slice refusing every
> allocation. `1` is already the library default; naming it keeps the level a property of the
> allocation.

Ascend takes one more under the same never-overwrite rule, `ENPU_DSMI_HOOK=1`, enabling a vendored
vcann-rt hook (`pack/gpustack-operator/external/ascend/vcann-rt/`) so the container's `npu-smi info`
reports its HBM **quota** and the slice's usage instead of the whole accelerator — the mixed view
NVIDIA gives, where `nvidia-smi` shows the virtual VRAM total while power and temperature stay
accelerator-wide.

NVIDIA takes one more under the same rule, `CUDA_DEVICE_ORDER=PCI_BUS_ID`. HAMi-core fills its limit
table from the `CUDA_DEVICE_MEMORY_LIMIT_<i>` keys in NVML enumeration order but reads a limit back by
CUDA ordinal, and the two coincide only under `PCI_BUS_ID` — CUDA's default orders by a performance
heuristic. The same invariant governs any integer a workload derives from an NVML index and hands to
CUDA, `CUDA_VISIBLE_DEVICES` included.

> **NVML is unaffected by it** — NVML always enumerates by PCI bus id, so the `Index` the DM reports
> matches `nvidia-smi` whether or not the variable is set. The ordering is stated where the injection
> is consumed, not where accelerators are detected.

> **Never-overwrite reads the container's own `env:` entries** — an `envFrom:`-sourced value is
> invisible to the allocator, so opting out that way needs an explicit `env:`.

### The order a positional injection is emitted in

A key addressed by position — `CUDA_DEVICE_MEMORY_LIMIT_<i>`, `HGGC_DEVICE_MEMORY_LIMIT_<i>`,
`VROCM_DEVICE_MEMORY_LIMIT_<i>`, `HSA_CU_MASK`'s `GPU_list` — is read against the numbering the
container itself uses, so every allocator emits its entries in one order: **ascending accelerator
index**, the enumeration the detector recorded.

Which vendors that order matters to differs, because it depends on who decides the container's
numbering:

| Vendor | Numbers the container's accelerators by | So the order is |
|---|---|---|
| NVIDIA | NVML/CUDA re-enumerating the visible cards by PCI bus id | **load-bearing** — the emission must match it |
| T-Head | the SDK renumbering the injected nodes by ascending card ordinal (measured) | **load-bearing** |
| AMD | `ROCR_VISIBLE_DEVICES`, which the injection itself states | self-consistent under any one order |
| Hygon | a `device_id` the operator itself writes into each `vdev<i>.conf` — meaning not yet established on hardware | positional, but **persisted**; see below |
| Ascend, Cambricon, MetaX, MThreads | not by position at all — the number travels as a value, or the request is single-accelerator | immaterial |

Where a number does travel as a value, it is the **driver's** index — dcmi's physical id, cnDev's
enumeration position — not the operator's logical one. The two coincide only while every accelerator
on the host was detected, so one failing a probe leaves every later accelerator carrying a logical
index below its driver index.

For Ascend the rule is **measured**, on a 910B2 against a simulated hole in the enumeration — the one
condition that makes the difference observable. All three sites carry the driver index with the
allocator driven directly on the host: `ASCEND_VISIBLE_DEVICES`, the `/dev/davinci<N>` the vendor
runtime then mounts, and a slice's `npu_info.config` `physical-npu-id`.

Through a device-manager DaemonSet under a real kubelet, the measurement covers the whole-accelerator
path only — the env and the mounted device node, against the ledger's own record of the accelerator it
allocated. The sliced site was not re-measured in-cluster; it rests on the host-side run, which drove
the same responder.

That measurement is a **V1** one, on a V1 driver. On a [V2 host](#ascend-two-dcmi-api-generations) the
physical id comes from a different entry point (`dcmiv2_get_chip_phy_id_by_dev_id`) and the rule is
**unmeasured**: nothing establishes that the driver numbers `/dev/davinci<N>` by that id there.

Two properties of the consuming side came out of the same runs: the container renumbers its
accelerators by ascending physical id, not by the order they were named in, so the emission order
stays immaterial even for a multi-accelerator claim; and an out-of-range value fails the container
start outright, which is why naming the wrong accelerator is a silent misplacement.

Hygon is the one whose position outlives the allocation: its figures go into `vdev<i>.conf` files on
the host, and a slot is reused only when the file at that path already names the same accelerator. A
sliced request is admitted for exactly one accelerator today, so the path is always `vdev0.conf` and
the mapping cannot move; if that gate is ever lifted, the order becomes a migration concern rather
than only a correctness one, because the files predate the allocation being retried.

The `Devices` ledger cannot be walked for that order. It keeps one group per accelerator model, so a
walk over groups is in enumeration order only within a group, and interleaves the models of a
container holding two. The group order is not part of the API either — both lists are declared as
maps keyed by identity.

The reconcile still stores them canonically (accelerators by index, groups by manufacturer and then
first accelerator), which keeps the stored list a function of the hardware rather than of which
detection pass first saw each group. The allocators order what they read regardless.

### Preflight: the preconditions read before a workload does

`device-manager preflight` reads, on a bare host, the allocation-time preconditions the allocator
reads when a workload lands. It drives each manufacturer's **own** responder with a synthetic
allocation request rather than a copy of it, so a preflight answer and the allocation it predicts
cannot disagree. The runbook is [Preflight Operations](../operation/preflight.md).

It asks three questions per manufacturer, in order, and each is answerable on its own:

1. **are the devices detected** — the detect pass, cross-checked against the host's own vendor CLI
   where one is established (NVIDIA, Ascend and AMD); for the other six the detect pass answers alone,
   and a count of zero is the container's own view rather than the host's;
2. **can they be sliced** — the driver read, then a container that is granted a slice and reports
   back the quota rather than the whole accelerator;
3. **can they be managed while sliced** — two named cases: *sidecar visibility*, where the owner's
   and the `sshd` sidecar's allocations are driven in the order the kubelet makes them and the second
   must name nothing the first was not granted (see [SSH-enabled Instances and the visibility
   resource](#ssh-enabled-instances-and-the-visibility-resource)); and *co-tenancy*, two independent
   slices on one accelerator each seeing its own quota.

**Every answer is one of three states**, exhaustive and mutually exclusive, each with a different
consequence for the allocation it guards:

| State | Meaning | What an allocation does |
|---|---|---|
| `ok` | the capability was read and the accelerator can serve the mode | proceeds |
| `unavailable` | the driver could not be asked — entry point missing, library not loaded, no privilege | is refused |
| `not-declared` | there is no such capability here to read or to set | proceeds without it |

**And carries the depth it was reached at**, so an assumption is never read as evidence:

| Depth | What was done | What it establishes |
|---|---|---|
| `declared` | the driver was asked and answered | what the host claims |
| `simulated` | the allocator's own code produced the artifact and it was asserted on, while nothing on the hardware changed | what the allocation would emit |
| `measured` | something ran and was observed | the behavior itself |

Nothing carries a deeper label than it earned. A case that could not be taken to the measured depth
is reported at the depth it reached, with the reason it went no deeper — never as a failure and never
as a pass.

Two of Q3's answers stop short by construction rather than by environment. Sidecar visibility is
answered at `simulated`: a measured one needs the owner's container still running, and every
container this starts is one-shot. A partition-backed accelerator is answered at `declared`:
reaching its visibility response means driving the capability that also creates a partition.

> **Why `simulated` is truthful** — several allocators write host state while serving an allocation,
> so the pass substitutes the manufacturer's own driver seam and redirects the paths that
> manufacturer alone knows it renders under. A manufacturer that serves an allocation out of paths
> and a resource request writes none of it to begin with, so it reaches this depth with nothing to
> substitute — all nine produce an injection, and the seam is only needed where producing one would
> otherwise touch the node.

It reaches the host by entering a bind-mounted host root with `chroot`, which gives it the host's own
container CLI — sibling probe containers with no runtime socket mounted — and the host's own vendor
CLI, which answers with no `/dev` mount in the container at all. That is what separates *this machine
has no accelerators* from *this machine has eight and your container cannot see them*. Preflight's
own code never runs in host context.

> **Why it is not part of `detect`** — `detect` is a pure read no flag can turn into anything else,
> while preflight starts containers that hold an accelerator and asks a driver to toggle a mode and
> put it back. On a node carrying live workloads that difference is worth a command of its own. The
> two cannot drift, because preflight reuses the detect pass rather than reimplementing it.

## Logical slicing per manufacturer

Every sliceable manufacturer has real per-slice runtime isolation, but only four — NVIDIA, Iluvatar,
Ascend and T-Head — take **both** budgets from a preload library. Every preload library is activated
through `/etc/ld.so.preload`.

| Manufacturer | Enforcer | Per-container quota and injection |
|---|---|---|
| NVIDIA, Iluvatar | HAMi-core `libvgpu.so` | `CUDA_DEVICE_SM_LIMIT` / `CUDA_DEVICE_MEMORY_LIMIT_*` |
| Ascend | vcann-rt `libvruntime.so` | an `npu_info.config` carrying `aicore-quota` / `memory-quota` |
| T-Head | the pair `hggc_quota.so` (enforcement) + `hgml_dlsym_hook.so` (visibility) | `HGGC_DEVICE_SM_LIMIT` / `HGGC_DEVICE_MEMORY_LIMIT_*` |
| AMD | `libvrocm.so` for VRAM, plus a hardware **compute-unit mask** the operator derives and ROCr enforces | `VROCM_DEVICE_MEMORY_LIMIT_<i>` in bare MiB + `VROCM_LEDGER_PATH`, and `HSA_CU_MASK` |
| MThreads | the host sGPU kmod | `MTHREADS_QOS_*` env vars — the compute share is a scheduling weight, not a hard cap |
| Hygon | the host DTK/hyhal runtime | a per-pod `vdev.conf` (a `cores%`-derived CU bitmask + VRAM cap) mounted read-only at `/etc/vdev/docker/` |
| MetaX | a sysfs `sgpu` subdevice | the accelerator is put in `sgpu` mode, a `cores%`-derived compute quota + VRAM cap written under a `fixed-share` scheduling class to `/sys/bus/pci/devices/<BDF>/sgpu/create`, then `METAX_SGPUS` plus the accelerator device nodes injected for the host MetaX runtime |
| Cambricon | a cnDev sMLU profile + instance | a profile with `mluQuota = cores%` and `memorySize` set to the VRAM budget is created or reused, a subdevice instantiated, its device nodes `/dev/cambricon_dev*` / `/dev/cambricon_ipcm*` / the instance node injected alongside the node-level control nodes, with a `VIRTUAL_DEVICES` env fallback for `--use-runtime` deployments since sMLU does not support CDI |

**Iluvatar reuses HAMi-core**, corex being CUDA-compatible. It keeps the accelerator visible through
`IX_VISIBLE_DEVICES` and needs `ix-container-runtime` to inject corex, so a sliced Iluvatar Pod must
carry `runtimeClassName: iluvatar` — without it the preloaded `libvgpu.so` finds no corex
`libcuda.so.1` to hook. HAMi-core-on-corex is verified at symbol level against a real corex driver
but **not on Iluvatar hardware**: advertised-and-injected, hardware-unvalidated.

**Ascend also turns on the driver's container-share mode** for each accelerator about to take a second
tenant — modes `sliced`, `shared`, `visibility`, never `exclusive`, which owns whole accelerators. The
allocator reads the flag through `binding/dcmi`, writing only when off, so an accelerator already
carrying a tenant costs one query. One whose flag cannot be set fails `Allocate` naming both
accelerator and flag, rather than admitting a pod that cannot use its device.

The read is classified into three outcomes, not treated as fatal on its own. A read reporting the dcmi
entry point missing — or a libdcmi that never loaded — refuses the allocation without writing, since
no `npu-smi` command adds an API the driver lacks, and that holds even for a device whose flag is
already on.

**A driver whose DCMI generation declares no such flag is allowed through instead**, in all three
tenanted modes. The [V2 API](#ascend-two-dcmi-api-generations) has no container-share entry point at
all, so there is nothing to read, nothing to write and no command to offer — the numbers such a
command would print are the binding's V2 addressing, not numbers an operator can type. Each allowed
allocation is logged with its accelerator.

> **Why** — the flag's refusal of a second container was measured on a 910B2 running V1, and it is not
> a universal rule. Whether the V2 generation enforces the same guard by other means is **unmeasured**;
> until it is, refusing would refuse every co-tenant allocation on that generation over an entry point
> its API does not define. The log line is the record of every allocation that relied on this.

Any other read failure still writes: the write is what makes the flag known, so a timeout cannot
refuse an allocation the write completes — it is logged instead. dcmi resolves each symbol
independently, so the absence can surface on the write rather than the read; that is refused the same
way, without a command. A write that fails for any other reason carries both reasons and the remedy.

Two properties of the flag:

- **Whole-accelerator allocation is unaffected** — measured on a 910B2 in both flag states, an
  exclusive container starts, sees full VRAM and opens the device identically.
- **Its one real effect is that the driver stops refusing a second container**, which is why
  `npu-smi` warns *"There are security risks when opening device sharing, Please ensure that only a
  single user uses the chip"* before setting it. Multi-tenancy on one chip is what logical slicing is
  for, and safety comes not from that guard but from GPUStack's own per-accelerator ledger (the
  [cross-mode invariant](#container-identification-and-cross-mode-exclusion)) plus vcann-rt's
  memory-quota enforcement, capping a slice at its `memory-quota` rather than the accelerator total.

The flag persists in the driver, so an accelerator that has hosted a tenant stays shareable until the
host reboots or an operator clears it with `npu-smi set -t device-share`.

**Cambricon needs the card in sMLU mode before a slice can exist on it**, and the allocator turns it
on — only for `sliced`, unlike Ascend's flag, since a whole-card tenant has no use for it. It reads
through `binding/cndev` and writes only when the mode is off, so a card already carrying a slice costs
one query, and it never turns the mode back off: that would strand the slices another pod is running.

A card whose library or driver has no sMLU API at all is refused, and without a command being
offered — none adds an API that is not there. The read normally reports it, so the card is never
written to; but cnDev looks the getter and the setter up independently, so the absence can surface on
the write instead, and that is refused the same way. Every other read failure still reaches the
write, the write being what makes the mode known; the read that could not be trusted is logged.

A write that fails for any other reason names the card by PCI address and cnDev index and hands over
`cnmon set -c <index> -smlu on`, telling the operator to confirm the ordinal with `cnmon` because that
equality is unverified. The mode then persists: once on, it stays on until an admin clears it with
`cnmon`.

Sliced Cambricon capacity is advertised on every card, whatever the host's driver or library can do.
Unlike Ascend, whose count depends on a vcann-rt runtime the image either ships for that family or
does not, Cambricon slicing needs nothing from GPUStack's own image — so there is nothing for the
detector to gate on, and a card whose mode is merely off must be offered anyway or the preflight that
turns it on could never run. Withholding is silent; an `Allocate` failure is loud.

So an advertised card is not a promise it can slice: the mode API and the profile API are looked up
independently, the `cntoolkit` userspace version is not readable from where the detector runs, and the
mode's effect on a whole-card tenant is unmeasured — Ascend's flag was measured benign in both states,
and no Cambricon equivalent exists. Each of those surfaces at `Allocate`, with the message above.

**AMD splits the two dimensions across two enforcers, alone among the manufacturers.** Memory is a
preload library like the others, accounting in a per-container region named by `VROCM_LEDGER_PATH`.
Compute is no variable at all: ROCm enforces it in hardware through `HSA_CU_MASK`, which ROCr reads
while initialising, before any preloaded code exists.

So the operator *derives* the mask — closed-form over the accelerator's topology, branching on its
GPU architecture family — and injects it; the library never sees it.

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

Because the mask is quantised to the accelerator's allocation atom, the **smallest requestable
percentage is a per-accelerator property**: 9 % on a 60 CU / 3 shader-engine part, 3 % on a
304 CU / 8 XCC one. A request below it is refused at allocation time, the message naming that
minimum, rather than rounded up into a ceiling nobody asked for. One above it that misses the atom is
aligned **down**, and the allocator logs the percentage delivered.

> **Admission does not know that minimum yet** — the webhook validates `1`–`100` and nothing
> publishes the per-accelerator floor, so a request below it is admitted, scheduled, then refused by
> the device plugin: the Pod fails to start and keeps failing. Until it is published, watch the very
> small request — on an accelerator with many shader engines, single-digit percentages may not be
> servable at all.

**T-Head emits the compute figure even at 100 %**, because that library refuses an accelerator whose
figure is missing rather than reading absence as "no cap".

Its sliced response carries **no** visible-devices env: like its other modes it passes the
accelerator's device node plus the two shared control nodes, adding only the library mounts, the
quota env and a per-container directory for the ledger region under the pod working directory — per
container, because the region is addressed by container-local accelerator index.

The visibility half makes the container's `ppu-smi` report its quota rather than the physical
accelerator, by interposing `dlsym`. A mounted `ppu-monitor` reads quota and usage for both dimensions
from the container's ledger region (`HGGC_LEDGER_PATH`), the only place the compute cap can be seen —
no `ppu-smi` field carries it.

**A workload image bringing its own `dlsym` interposer through `LD_PRELOAD`** — processed before
`/etc/ld.so.preload` — leaves that half loaded but never entered: the quota still applies, but
`ppu-smi` shows the whole accelerator. The library cannot detect this, so it is a caveat, not an
error.

### Where the preload libraries come from

They are compiled into the operator image per runtime version — cloned inline at pinned commits, no
git submodule, in the `xbuild-nvidia-cuda-*` / `xbuild-ascend-cann-*` Dockerfile stages, scripts
under `pack/gpustack-operator/external/{nvidia,ascend}` — and staged onto the host
(`/var/lib/gpustack/operator/lib`) by a device-manager **init container**.

The allocator mounts the matching library plus a per-pod working directory into each sliced
container, reclaiming those directories once their pods are gone.

**Iluvatar adds no build stage of its own** — its lib dir is filled by copying the
`xbuild-nvidia-cuda-12` HAMi-core `/out` a second time (corex exposes a CUDA-compatible
`libcuda.so.1`, so the same library serves), one flat directory, no runtime-version subdivision.

**AMD needs exactly one build stage, a property of the library rather than a shortcut.**
`libvrocm.so` links no ROCm object: every runtime entry point resolves at load time, not link time,
so one artifact serves every ROCm version a workload container may bring, and
`${GPUSTACK_LIB_DIR}/amd/` is flat where `nvidia/` and `ascend/` carry a subdirectory per runtime
generation.

It is built from this repository's own `csrc/amd/rocm-slicing-shim` tree, in a ROCm devel image
chosen for its glibc floor rather than its ROCm version, and ships the two readers (`rocm-monitor`,
`rocm-cumask-check`) the allocator mounts beside it. ROCm publishes no `aarch64` user space, so the
**`arm64` operator image carries no AMD shim** — and no AMD node either, the detector's libraries not
loading there.

**T-Head's pair is the exception on both counts.** It is built from this repository's own sources
(`csrc/thead/ppu-slicing-shim`, by the `xbuild-thead-ppu` stage inside the manufacturer's SDK image)
rather than cloned from an upstream at a pinned commit, and carries no runtime-version subdirectory,
the PPU SDK living in the workload container rather than ours.

That SDK is `x86_64`-only, so the **`arm64` operator image carries no PPU shim** — the detector does
not check for one, on the ground that a PPU only exists in an `x86_64` host.

## SSH-enabled Instances and the visibility resource

For an **SSH-enabled Instance** the workload runs in a two-container Pod: `main` (the user image) and
`sshd` (an Alpine sidecar that `nsenter`s into `main`). The accelerator request and its
runtime-isolation artifacts go on `main`, where the workload — and the SSH shell entering `main`'s
namespaces — runs. `sshd` requests an internal-only
`device.gpustack.ai/<manufacturer>.visibility` resource, quantity = `main`'s accelerator count.

The allocator serves it from the same `ResourceServer` under an internal `Visibility` mode:
`Allocate` selects no fresh device, reuses the physical device(s) `main` holds, and returns the same
plain response the non-sliced modes do — the manufacturer's visible-devices env, and for those that
inject their own nodes ([Exclusive and shared](#exclusive-and-shared)) those nodes as well — with no
slicing artifacts and no ledger consumption. It correlates the two calls in two steps:

- the in-process, pod-keyed reservation recorded at `main`'s `Allocate` — the kubelet allocates
  `main` before `sshd`, sequentially, in Pod spec order;
- failing that, the Pod's durable `device.gpustack.ai/accelerator.allocated` annotation, so a
  device-manager restart landing between the two calls no longer strands the sidecar.

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

The trigger is the owner container's own `.partitioned.<kind>-<profile>` request, in the Pod spec
from the start.

The identity comes from the manufacturer responder's partition capability
(`PhysicalSlicedResponder` — the interface that materializes a partition, so a responder able to
carve one can name it).

It reads the owner's durable node-local ownership record and proves the recorded instance still live
before naming it — see [NVIDIA MIG](../operation/nvidia-mig.md#requesting-a-mig-instance) and
[T-Head MIG Operations](../operation/thead-mig.md#requesting-a-partition).

A responder lacking that capability, or unable to substantiate the identity, fails the admission
closed rather than widening the grant back to the accelerator.

The visibility resource is advertised per accelerator as a pool of `SlicedResourceMaxSize` tokens
outside the known-acceleratable families, so it never gates scheduling and admission never reads it
as a second accelerator mode. Every accelerator backend registers it.

The per-accelerator AdmissionCheck ([Admission](admission.md)) re-checks feasibility only **before**
admission: an admitted Workload's allocation is already in the ledger, so re-evaluating would count a
slice against itself.

## Container identification and cross-mode exclusion

`Allocate` carries the assigned device IDs but not the pod identity. The allocator therefore matches
the node's pending pods requesting the resource: it drops candidates this call could not serve on the
accelerators offered (a slice demanding more per accelerator than their remaining), skips a (pod,
container) already holding a reservation, and takes the oldest survivor — those left are
interchangeable.

The feasibility test **disambiguates, it does not gate**: the ledger lags reality, so an
all-infeasible set falls back to the unfiltered oldest rather than failing a resolvable request.
Admission belongs upstream, to the Pod webhook and the AdmissionCheck.

All `Allocate`s of a node run in its single device-manager process, so a per-node mutex serializes
each workload `Allocate`'s *identify → cross-mode check → reserve* section; the durable-annotation
patch runs outside it.

Reservations key on **(Pod UID, container)** and the durable
`device.gpustack.ai/accelerator.allocated` annotation is a **map keyed by container name**, so two
containers of one group each holding a live claim are both recorded and both charged. An entry
charges its accelerator until its **Pod** is gone — the reclaimer and the kubelet also scope a device
to the Pod's life, not the container's.

One thing takes an entry back earlier, and it is the only one: an allocation the manufacturer
responder refuses **after** the record is written is given back on the spot — the entry and the
reservation both — because the kubelet does not start that container. A claim the container already
held is restored rather than dropped, and a give-back that cannot reach the API is retried until it
lands or the Pod is gone.

**(a) This enforces the per-accelerator exclusive/shared/sliced cross-mode invariant.** An
accelerator kubelet assigned that another mode holds, per the ledger `Status` or the in-process
reservation, is refused with `FailedPrecondition`: an exclusive tenant truly owns its accelerator on
every path, Kueue or raw.

Prevention runs a stage earlier. `ListAndWatch` keeps an accelerator held in another mode advertised
— removing tokens would strand kubelet's checkpointed allocations — but reports its tokens
**Unhealthy**, read from the ledger `Status` and the reservation and pushed on the same
reservation/release instant, so the kubelet can never hand a held accelerator to an opposite-mode
pod.

It picks tokens freely: `GetPreferredAllocation` *does* run under the default TopologyManager policy
`none`, but is advisory and may be ignored. `Visibility` is exempt: the `sshd` sidecar's token must
stay allocatable on the very accelerator its workload holds, whatever mode that hold is.

**(b) It also maps a batch of identical GPU Pods admitted together (e.g. by Kueue) one-to-one to
distinct pods**, keeping annotations and the ledger correct instead of double-attributing one and
losing another.

The `sshd` visibility path re-finds its Pod's **non-self accelerator allocation** — reservation
first, durable annotation second, both by the same owner pick — rather than the reservation-skip; the
request rules confine a Pod's claims to one container group, so that owner is unambiguous.

## Placement is a preference, not a decision

**For the accelerator-bound families**, tokens name an accelerator, so the kubelet's pick of a token
*is* the pick of an accelerator; the plugin only orders the candidates it offers back from
`GetPreferredAllocation`.

For a **logical slice** it offers the tokens of the **most-occupied accelerator that still fits**: one
already serving slices beats a pristine one, ties broken by the accelerator's position within its
group, so identical requests against identical state place identically. Slices coalesce instead of
each opening a fresh accelerator and stranding a node whose every accelerator is partly used but none
can host one large claim.

The ordering is computed **per `DevicesGroup`**, walking the groups in spec order, not across a
node's groups at once.

It stays a *preference*: the per-accelerator fit filter lives **only** in this advisory response —
`Allocate` refuses an accelerator another mode holds, but never one merely short of room — so an
accelerator's VRAM budget is respected exactly insofar as the kubelet consumes the hint, with no
backstop below it. Two properties are load-bearing:

- every id returned must be one the kubelet actually offered — the full
  `<group>:<accelerator>:<token>` form, since an id the kubelet cannot match is discarded
  **silently** and the accelerator choice degrades to arbitrary;
- the call stays advisory by API contract, so under a restrictive TopologyManager policy the kubelet
  allocates the NUMA-aligned set before consulting the plugin at all.

### The partitioned family: fungible tokens

The **`Partitioned`** family is the one exception to accelerator-bound tokens. Its `Allocate` treats
the kubelet's device IDs as a *quantity* and chooses the accelerator itself, under the same mutex,
against the live geometry — publishing the choice (accelerator, profile, intended memory-slice
intervals) into the reservation before releasing the mutex, so a concurrent call selects against
post-decision state.

Accelerators are **packed, not spread**: the most-occupied one that still fits wins, keeping a
sibling whole for a later whole-accelerator profile. A retried `Allocate` for a container that
already has an allocation reuses the accelerator it used, read from the reservation and then from the
durable annotation.

Because no partition token names an accelerator, that family's health is a pure node-level count of
remaining room — `allocated + remaining` published over a stable set of IDs, never removing an ID a
live allocation holds — and it reports **no** NUMA topology, since the kubelet would otherwise align
CPU and memory to an accelerator the plugin may not use.

One residual is stated rather than solved: a partition an administrator carves out of band is
invisible to every annotation-derived key, so hand-carving on a managed node is unsupported (see
[Accelerator Requests](../accelerator-requests.md#limitations)).

## One driver stack per node

A node hosts a single driver/runtime stack per manufacturer, so every `DevicesGroup` of a given
manufacturer on it shares one driver and runtime version. The per-runtime-version library subdir
(`cuda-<major>` / `cann-<major>-<family>`) the allocator picks from the first allocated accelerator is
therefore correct for every accelerator in a sliced allocation.

**Nothing below the detector re-checks that such a subdir was actually built.** So on Ascend the
detector offers logical slicing only for the family/runtime-major pairs the image ships a vcann-rt
for — one per `xbuild-ascend-cann-<major>-<family>` stage. A pair with no stage is not advertised at
all, instead of being advertised and then failing to start the container on a missing directory.
Adding a build stage widens that set.

The allocator still guards against a mismatch **defensively** (NVIDIA rejects a sliced allocation
spanning different CUDA majors; Ascend rejects a multi-accelerator sliced allocation, since
vcann-rt's `npu_info.config` models a single physical NPU), so any future regression fails the
`Allocate` loudly instead of silently mounting an incompatible library.

---

**See also** — [Accelerator Requests](../accelerator-requests.md) (the resource keys these families
serve) · [NVIDIA MIG Operations](../operation/nvidia-mig.md) · [T-Head MIG
Operations](../operation/thead-mig.md) · [Settings](../settings.md)

**Next** → [Scheduling Chain](scheduling-chain.md) — how these labels and the ledger become Kueue
objects.
