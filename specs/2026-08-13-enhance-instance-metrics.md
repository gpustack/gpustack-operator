# Spec: Enhance Instance Metrics — Sliced Compute and Memory Observability

Status: Shipped
Type: Feature

## Summary

An Instance that holds a *slice* of an accelerator cannot see its own utilization. Both surfaces
reporting Instance metrics — the `instances/<name>/metrics` subresource and the Device Manager's
Prometheus exporter — report the **whole card's** figures for every allocation, so a logically sliced
or hardware-partitioned Instance shows its neighbours' consumption as its own and any percentage
computed from the response describes the card rather than the slice. Both also read the kubelet, the
metrics API and every allocated manufacturer's Device Manager for an Instance none of whose containers
has started, spending three upstream reads to produce either a `503` or another tenant's numbers.

This spec makes a slice's own **compute and memory utilization observable**, and it does so by
restating what an `accelerators[]` entry means rather than by adding a second place to look:
**every entry reports what the Instance was granted and what it is using of that grant, in every
mode**. A whole device reads the device; a logical slice or a hardware partition reads that share's
quota and that share's usage, measured through the manufacturer's own per-process device query and
attributed to the Instance by host PID — one mechanism across eight of the nine supported
manufacturers rather than one bespoke reader per slicing shim. The Device Manager attributes and
aggregates per Pod before publishing, so raw PIDs never leave the node. And both surfaces gate on the
backing Pod having a started container, so an Instance that is not running yet answers `200` with
declared totals and measured-zero usage and performs no upstream read.

## Motivation

### Goals

- **G1 (primary)** A slice Instance's response alone yields **its own** memory and compute
  utilization — no reading its spec, no knowledge of its neighbours. This is the main contradiction
  the spec exists to resolve; everything else serves it.
- **G2** A query against an Instance with no started container performs **zero** upstream I/O and
  returns a stable `200` shape rather than a `503`.
- **G3** One implementation behind both surfaces, field for field, preserving the invariant
  `pkg/kubemetrics` was created to hold — within the scope each surface can actually cover (F1).
- **G4** **One field name, one meaning, in every mode.** A consumer asking "what is this Instance
  using" reads one metric whatever the allocation did; it never branches on the mode to pick a field,
  and never has to know the mode at all. `mode` exists to group and filter by, not to choose with.
- **G5** The usage mechanism is **uniform across manufacturers**: adding a vendor costs a binding call,
  not a new binary-layout reader.
- **G6** A figure that cannot be measured is **absent, never `0`**; zero means measured and idle. The
  *reason* for an absence is discoverable on the exporter surface (a capability gauge) and in the
  Device Manager log. The subresource omits the field without a reason — an explicit decision, not an
  oversight: see Alternatives.

`docs/reference/instance-metrics.md` states the gap in its own Limits section — "*Whole accelerators
only. A shared or sliced allocation shows the whole accelerator's figures; per-pod GPU attribution
does not exist.*" That sentence is the feature request. `InstanceMetricsHandler.OnGet` never looks at
the Instance's phase: it guards three *backing-state* conditions and answers `503` for each, while
`Starting` and `Stopping` fall straight through to a kubelet read plus one Device Manager fetch per
allocated manufacturer.

### Non-Goals

- **MThreads is not covered.** Verified against upstream `mtml_2.2.0.h`: no per-process API exists at
  all — the only pid-bearing type is codec-session metrics (video encode/decode), and every memory
  entry point is device-level. Its slicing is enforced by the vendor's own QoS environment
  (`MTHREADS_QOS_MEMORY_LIMIT`, `pkg/devicemanager/allocator/mthreads/deviceplugin.go:189`) rather
  than by a shim of ours, so there is no region to read either. No hardware available. Revisit only if
  a later `mtml` adds the API.
- **Compute utilization for a hardware partition.** NVML states it outright: "*On MIG-enabled GPUs,
  querying process utilization is not currently supported*" (`binding/nvml/nvml.h:7825`, repeated for
  the newer entry point). Partition *memory* is in scope (F10); partition compute is not obtainable
  and `coresUtilizationPercent` stays absent for it.
- Host-boundary isolation of the slicing shims' shared state — `HostIPC`, the node `/dev/shm`, the
  Ascend `virtual-npu-id` invariant, `additionalVolumes` shadowing a shim path. Those are isolation
  defects rather than reporting gaps, and are tracked by a companion spec not yet in this tree.
- History or time series. Both surfaces still report one current sample.
- Replacing any upstream slicing shim. This spec reads what the *manufacturer* reports, so it is
  independent of which shim enforces the quota.

## Proposal

An `accelerators[]` entry is **the Instance's own view of what it holds**. `memoryTotalMiB` is what
the Instance was granted — the device's capacity, a logical slice's quota, or a hardware partition's
own capacity — and `memoryUsedMiB` is what it was measured holding *of that grant*, from the
manufacturer's own per-process query summed over the processes belonging to this Instance.
`coresUtilizationPercent` is likewise against the Instance's own compute allowance, so a slice capped
at a fifth of a card and saturating that fifth reads `100`. The entry gains `mode`, which names how
the grant was made without changing what any figure means, and its `id` becomes the identifier of
what was actually granted — a MIG UUID for a partition rather than the parent card's.

The whole card's own readings that are not resource usage — temperature, power draw and health —
stay whole-card in every mode, because a share of a card has none of its own.

### User Stories

#### Story 1
As a console developer, I want a sliced Instance's detail page to read "6.1 / 20 GiB VRAM, 18% / 25%
compute", so that the user sees their own slice's occupancy rather than the card's.

#### Story 2
As a user, I want a freshly created Instance that is still pulling its image to answer `200` with
zeros, so that the frontend needs no `503` branch and never shows me a neighbour's usage as my own.

#### Story 3
As an operator, I want one Prometheus query — no `or` fallback, no per-mode branch — to show every
Instance's own accelerator usage, with `mode` available to group by, so that I can judge whether an
oversubscription is safe.

#### Story 4
As an operator on a driver that cannot answer the per-process query, I want the field **absent** with
a gauge naming the reason, so that "could not measure" is never read as "idle".

#### Story 5
As an operator running a mix of whole-card, sliced and MIG Instances, I want one dashboard panel to
work for all of them, so that adding a partitioned pool does not mean rewriting every query.

### Core Features & Acceptance Criteria

#### F1 — The gate: has any container started?

The gate is **whether any of the Pod's containers has started**, not whether they are ready.

Readiness is the wrong predicate and would fabricate measurements: a Pod can be `Running` with a
failing readiness probe, an unready sidecar, or a termination already begun, while its main container
is holding accelerator memory. Reporting `0` then is a false idle reading and contradicts G6. "No
container has started" is a different claim, and under it zero is a *measurement* — nothing exists
that could consume anything.

- No started container → `200`, `timestamp` at read time, the three totals populated, the three
  `used` figures **`0`**, `accelerators` omitted entirely.
- No started container → **zero** requests to the kubelet, to `metrics.k8s.io`, and to every Device
  Manager. Asserted by a fake that fails the test if called.
- Any started container → measured normally, ready or not.
- **Subresource only:** with no Pod of its own (Stopped, Deleting, unscheduled, or only a previous
  incarnation's Pod), the totals come from the Instance's `spec.resources`. **All three of today's
  backing-state `503`s become `200`.**
- `503` survives in one case only: a Pod with a started container whose every measurement source
  failed. The message keeps naming which source failed and how.
- **The exporter cannot cover the no-Pod case, and says so.** It discovers Instances solely by listing
  Pods indexed to its node (`pkg/devicemanager/exporter/poller.go:250-281`), and the controller
  replaces a stopped Instance's whole status with `Phase: Stopped`, clearing `NodeName`
  (`pkg/worker/controllers/worker/instance.go:183-202`) — so no node owns it and none can discover it.
  The exporter therefore applies the started-container gate to the Instances it can see and documents
  the Pod-less gap, exactly as it already documents having no series for an accelerator-less node.

#### F2 — The grant is the total, every covered manufacturer, both slicing kinds

- `accelerators[].mode` carries the allocation mode, in every mode including the whole-device ones.
- `memoryTotalMiB` is **the grant**, derived not measured, and its source depends on the mode:
  - `Exclusive`, `Shared`, `Visibility` — the device's own capacity, exactly as today.
  - `Sliced` — `Allocated / nodefeature.ResourceMaxUnits × cardVRAMMiB`, where `Allocated` comes from
    the allocation the device plugin recorded (`pkg/deviceplugin/server.go:1285`, basis
    `nodefeature.ResourceMaxUnits = 1_600_000`). This is the **same memory-anchored basis admission
    charged credits on**, so the reported total cannot disagree with the granted quota. A granted
    quota never folds back to nothing: the floor is 1 MiB.
  - `Partitioned` — the **partition's own capacity**, read on the partition's own device handle (F9).
    Not folded out of the units and not parsed from the profile name, both of which round: a
    `1g.10gb` of an H100 carries 9856 MiB, where the units say 10240 and the name says the same.
- `id` is what the Instance holds: the device's identifier, or under `Partitioned` the partition's own
  — a MIG UUID rather than the parent card's.
- `temperatureCelsius`, `powerUsageWatts` and `unhealthy` stay **whole-device** in every mode.
- A total is absent only when nothing can state the grant — a partition whose handle could not be
  read, or a device whose capacity did not reach the sample.

#### F3 — Slice usage from the manufacturer's per-process query

One mechanism everywhere: ask the vendor library which processes hold the device and how much, keep
the ones belonging to this Instance.

| Manufacturer | Binding | Per-process entry point | Mem | Compute |
|---|---|---|---|---|
| NVIDIA | `nvml` | `nvmlDeviceGetComputeRunningProcesses_v3` | ✓ | `nvmlDeviceGetProcessUtilization`, with the `…ProcessesUtilizationInfo` fallback |
| Iluvatar | `ixml` | `nvmlDeviceGetComputeRunningProcesses` | ✓ | **none** — the vendored header exposes process memory but the binding exposes no process-utilization path |
| AMD | `rsmi` | `rsmiComputeProcessInfoGet`, `…GpusGet`, `…ByDeviceGet` | ✓ | ✓ `cu_occupancy`, sentinel-gated |
| Hygon | `rsmi` | `rsmiComputeProcessInfoGet`, `…ByPidGet` | ✓ | ✓ `cu_occupancy`, sentinel-gated |
| T-Head | `hgml` | `hgmlDeviceGetComputeRunningProcesses_v3` | ✓ | ✓ `hgmlDeviceGetProcessUtilization` — the OLDER symbol, because the versioned one is an exported stub on the driver measured on; see the hardware table |
| Cambricon | `cndev` | `cndevGetProcessInfo` | ✓ | ✓ `cndevGetProcessUtilization` |
| Metax | `mxsml` | `mxSmlGetSingleGpuProcess_v3` | ✓ | — |
| Ascend | `dcmi` | `dcmi_get_device_resource_info` — needs F6 | ✓ | see F6 |

- `memoryUsedMiB` is the sum over the Instance's processes on that device, and
  `coresUtilizationPercent` is their measured share of the card **restated against the Instance's own
  compute allowance** — the cap the allocator enforced on the container, so a slice at its ceiling
  reads `100`. Both are **absent — never zero** — where the manufacturer offers no entry point and
  where the driver answers `NOT_SUPPORTED`. **A carved share whose usage could not be measured
  reports no usage at all, never the device's**, whose figure counts every other tenant on the card.
- **The restatement rounds up and is not clamped.** The manufacturers measure the card in whole
  percent, so a small cap makes the numerator coarse and flooring would publish a measurably busy
  slice as idle; and a figure above 100 is a cap that is not being enforced, which clamping would
  hide. Both are stated in the docs rather than smoothed away.
- **An unattributable row poisons the device, not just itself.** If any row returned for a device
  cannot be attributed to a known Pod, every slice figure on that device is **absent** for that
  sample. Dropping the row instead would publish a partial sum as a complete one — and if the dropped
  row was the Instance's only process, publish a plausible measured zero.
- The **sampling semantics are vendor-specific and must be honoured, not flattened**:
  - NVML returns only **non-zero** utilization samples and can retain **several timestamped samples
    per PID**; only the newest valid sample per PID contributes — which is what T-Head's own compute
    controller already does (`csrc/thead/ppu-slicing-shim/hggc/hggc_compute.c:270-334`).
  - NVML's process record can carry `NVML_VALUE_NOT_AVAILABLE` in the memory field
    (`binding/nvml/nvml.h:290-306`); that is a sentinel, not a number.
  - Sums are taken in the vendor's **native units** and converted once at the end, so a total below
    1 MiB does not truncate to zero.
  - A driver reporting more rows than the accepted bound is **truncated**, and a truncated read makes
    the field absent rather than partial.
- **Metax is read through `mxSmlGetSingleGpuProcess_v3`, not `mxSmlGetProcessInfo_v3`.** The latter is
  not a per-device call and its process count is input-only, so a driver returning more rows than the
  buffer holds signals nothing at all — leaving no way to tell a complete list from a truncated one,
  which is the one distinction this whole feature rests on. The per-device call carries a real in/out
  count.
- A hardware partition takes the same path but on the partition's own device handle — see F9.

#### F4 — Attribution: which processes are this Instance's (prerequisite)

The vendor libraries return **host** PIDs. Mapping those to an Instance is new work — **nothing in the
repository does it today** (no cgroup parsing anywhere; `/proc/<pid>` is read only by
`pkg/utils/signalx`), which is why this is the first task and gates the shape of F3, F5 and F10.

- The Device Manager runs with `hostPID: true` and already lists the node's Instance pods, so the
  mapping is `/proc/<pid>/cgroup` → the pod UID kubelet puts in the cgroup path, on cgroup v1 and v2
  and under both the systemd and cgroupfs drivers.
- **Container identity is required, not optional.** F7's section is keyed by (pod UID, container,
  device ID) because allocations are persisted per container and only later aggregated Pod-wide
  (`pkg/deviceplugin/controller.go:891-913`) — a pod UID alone cannot say which container's allocation
  owns a process, and the "a sliced container holding more than one accelerator yields no figures"
  invariant cannot be evaluated without it. The container ID is parsed from the cgroup path and joined
  against `Pod.Status.ContainerStatuses`; a join that fails yields **no container-keyed result** and
  never guesses `main`.
- **Reject, never guess.** An ambiguous path (two pod ancestors, cgroup v1 controllers naming
  different pods, a pod UID appearing only as a substring of another segment) is unattributable.
- **PID reuse cannot be fully closed and is bounded, not ignored.** A vendor row carries a numeric PID
  and no process-start identity, so a PID recycled between the vendor query and the cgroup read would
  attribute to the wrong Pod. The identity is re-read after the query and any change rejects the row;
  the residual window is the time between the two reads and is documented rather than claimed away.
- A PID that cannot be attributed makes its whole device's sample absent (F3), so this failure is
  loud.

#### F5 — A capability probe, so a missing figure has a named reason

The Device Manager records, per **device** and per entry point, whether the call actually answers on
this node's driver, and publishes it as a gauge. Per device, not per manufacturer: a node with mixed
support would otherwise emit two samples with identical labels, which makes a scrape invalid.

The failure is real and silent: on Blackwell with driver 580.159.03,
`nvmlDeviceGetProcessUtilization` **and** `nvmlDeviceGetComputeRunningProcesses` both return
`NVML_ERROR_NOT_SUPPORTED`. Without a probe, a whole GPU generation reports absent slice usage with no
way to tell that from "nothing running".

- The reason taxonomy covers more than `NOT_SUPPORTED`. As built, the tokens are: from the device
  query, `unsupported`, `permission`, `transient_driver_error`, `truncated`; from the section itself,
  `invalid_data` (the vendor's rows contradict the card), `bounded` (the device was dropped whole to
  keep the section inside its size bound) and `version_skew` (the producer wrote a schema this
  consumer cannot read); and from attribution, one token per way a row can fail to name an Instance —
  `exited`, `permission`, `unreadable`, `zombie`, `unstable`, `pid_namespace_invisible`, `mediated`,
  `no_pod_component`, `ambiguous_pod`, `no_container_component`, `ambiguous_container`, `unknown_pod`,
  `unknown_container`. The attribution set is deliberately finer than the `unattributed` /
  `ambiguous` / `container_join_failed` this section first sketched: those three would have folded a
  race, a host process and a mediating daemon into one label, and which of them it was is exactly what
  an operator reading an absent figure needs. Staleness is not a reason but a rule — a snapshot older
  than three monitor periods yields nothing at all, so there is no figure left to carry one.
- A probe result is **not** a freshness claim: support alone never implies the current sample
  succeeded, and re-detection is triggered only by manufacturer/device/health changes
  (`pkg/devicemanager/detector/detector.go:137-145`), so the probe is refreshed on the monitor period
  rather than only at detection.

#### F6 — Close the Ascend binding gap

Verified on Ascend hardware (8 NPUs, driver / `npu-smi` 25.5.1): the per-process figure exists and is
live — `npu-smi info -t proc-mem -i 0 -c 0` reports
`Process id:899293  Process name:VLLMWorker_TP  Process memory(MB):57280`. The gap is entirely on our
side, in two layers:

1. **`dcmi_get_device_resource_info` is not wrapped.** It is declared in the vendored
   `binding/dcmi/dcmi_interface_api.h`, `struct dcmi_proc_mem_info { int proc_id; unsigned long
   proc_mem_usage; }` is already generated as `ProcMemInfo`
   (`binding/dcmi/zz_generated.types.go:259`), and `libdcmi.so` exports the symbol — but
   `binding/dcmi/dcmi_wrapper_api.def` wrapped only 29 functions and this was not one of them.
   **No header refresh is needed to reach it.** Measured against the driver 25.5.1 header on the
   host: our 784-line vendored copy declares this function with a signature identical to the host's
   2332-line one, and `dcmi_proc_mem_info` with it. The whole gap is the one missing `.def` line, so
   refreshing the header would be 1548 lines of new declarations bought for nothing — and
   regenerating from them risks changing an existing generated signature, which is the one thing
   this work must not do.
2. **A per-vNPU figure would be better than a per-process one, and it is not reachable.**
   `dcmi_computing_resource` (vendored header line 278) carries `vdev_aicore_utilization`,
   `vdev_memory_total` and `vdev_memory_free` — per-**vNPU** compute *and* memory, exactly what a
   slice is on Ascend. But it is only ever an embedded field, and **no function takes the struct that
   wraps it** (`dcmi_vdev_query_stru`) in *either* header, nor does `libdcmi.so` export one: its
   `vdev` symbols are `create`, `destroy` and `mode` only. There is no query entry point to call.

So Ascend ships per-process **memory** and leaves `coresUtilizationPercent` **absent**,
explained by F5 with reason `unsupported`. That closes the F6 layer-2 question for good, on hardware
evidence rather than on a header reading.

**The library's process count is an output-only parameter, and that is a hazard to contain.** Probed
on the host: `dcmi_get_device_resource_info` ignores the count passed in — a count of `0` still had
row 0 written and the count overwritten with the real one — so the library cannot be told how large
the caller's buffer is, and a device holding more processes than the buffer would have it write past
the end. The read therefore over-allocates with a **guard tail** left for the library to overrun
into: a written guard row means the device held more processes than a read accepts, which is reported
as an incomplete read rather than as a shorter list, and the overrun is confined to memory the
binding owns. Returning the rows that did fit would present part of a device's processes as all of
them — plausible, and wrong.

#### F7 — The Device Manager is the producer, and the snapshot carries an aggregate

- The vendor query and the attribution are node-local, so the Device Manager's monitor loop performs
  them. `MonitorSnapshot` gains a **dedicated slice section** keyed by (pod UID, container, device
  ID) — not a field inside `device.AcceleratorMetrics`, which stays what it is: one
  card's readings. Raw per-process rows are producer-internal and are **stripped before the snapshot
  is stored**; the wire carries only the per-Pod aggregate.
- The section carries a **schema/feature version**. Without it a newer worker reading an older Device
  Manager sees a valid, fresh snapshot with no slice data and cannot tell version skew from
  unsupported hardware — which would violate G6. Skew reports absent with reason `version_skew`.
- The section carries **non-sensitive diagnostic counts** per device: rows returned, attributed,
  known-non-Instance, unreadable, ambiguous, truncated. Without them an aggregate-only snapshot makes
  undercounting indistinguishable from genuine idle.
- The aggregate is bounded so it cannot push the response past the worker's read limit
  (`_MonitorSnapshotMaxBytes = 4 << 20`, `pkg/worker/extensionapis/worker/instance.metrics.go:53-55`);
  the bound is asserted in a test rather than assumed from typical node sizes.
- **This increases what an unauthenticated endpoint exposes, and that is documented rather than
  waved away.** `/monitor/snapshot` performs only method checking
  (`pkg/devicemanager/snapshot.go:12-41`), the chart's NetworkPolicy is **off by default**
  (`deploy/gpustack-operator/chart/values.yaml:283-305`), and the worker dials it with certificate
  verification disabled. Adding pod UID, container name and per-tenant usage materially widens that
  surface. The reference page states it plainly and recommends enabling the NetworkPolicy on a
  multi-tenant cluster; changing the default or adding authentication is a separate decision.
- The existing staleness rule governs: a snapshot older than three monitor periods yields nothing
  (`detector.MonitorSnapshotFresh`).
- **A container holding several carved accelerators yields one block per accelerator, and that is not
  an error.** Admission pins a logical slice to one accelerator today, but that is a current
  restriction rather than an invariant — a single container requesting several logical slices is a
  direction this project intends to open — so nothing here enforces the restriction, and no figure is
  suppressed on account of it. The section's (pod, container, device) key and the API's per-device
  `accelerators[]` already express the plural case, so the feature it is waiting on needs no change
  here. What *is* refused is two **containers** of one Pod carving the same device: `accelerators[]`
  is keyed by device, so one entry cannot carry two containers' grants, and the entry reports no
  figures of its own rather than one of them picked or the two summed. A `Visibility` sidecar is not a
  second grant — it sees what its sibling holds — and does not trigger this.

#### F8 — Exporter parity

No new families. The seven `gpustack_instance_accelerator_*` families now report the Instance's own
grant and usage in every mode, and gain a **`mode` label** so a query can group or filter by the
allocation without choosing a different metric name for it:

| Family | Unit | Sample field |
|---|---|---|
| `gpustack_instance_accelerator_memory_total_mib` | MiB | `memoryTotalMiB` |
| `gpustack_instance_accelerator_memory_used_mib` | MiB | `memoryUsedMiB` |
| `gpustack_instance_accelerator_memory_utilization_percent` | 0–100 | `memoryUtilizationPercent` |
| `gpustack_instance_accelerator_cores_utilization_percent` | 0–100 | `coresUtilizationPercent` |
| `gpustack_instance_accelerator_temperature_celsius` | °C | `temperatureCelsius` |
| `gpustack_instance_accelerator_power_usage_watts` | W | `powerUsageWatts` |
| `gpustack_instance_accelerator_unhealthy` | 0 or 1 | `unhealthy` |

An absent figure is not published; a total is published whenever the allocation can state it. Same
gate. Plus the F5 capability gauge — `gpustack_accelerator_process_capability`, labelled per device
and entry point.

> A `mode` **label** rather than a `…_slice_*` **family** is the whole point of the expression. Every
> series in these families is one quantity — what this Instance was granted and is using — so a label
> partitions it correctly and `sum by (mode)` is meaningful. Separate families would make the metric
> name depend on the allocation, which forces an `or` fallback into every dashboard and risks counting
> a carved Instance twice in any `sum()` already written against the card families.

#### F9 — Documentation

`docs/reference/instance-metrics.md` states the one-entry-per-grant expression and what each field
means in every mode, the per-manufacturer availability matrix naming which manufacturers are
hardware-verified and which are not, the capability probe and its reasons, the started-container gate
replacing the three backing-state `503`s in *Degradation rules*, the exporter's Pod-less gap, the
snapshot exposure note, and a *Querying it* section that is one metric name rather than a fallback.

#### F10 — Hardware partitions: resolve the instance handle, report its identity and memory

A partition's usage lives on the partition's own device handle, not the parent card's — NVML requires
the specific MIG handle for per-instance process memory. The allocation records a profile name and
memory-slice placements (`AllocatedPhysicalProfile`, `AllocatedPhysicalPlacements`), not a handle, so
the handle is resolved by **reverse lookup**, which is feasible and cheap:

- `nvmlDeviceGetMaxMigDeviceCount` + `nvmlDeviceGetMigDeviceHandleByIndex` enumerate the instances;
  `nvmlDeviceGetGpuInstances` / `nvmlGpuInstanceGetInfo` yield each one's profile and placement, and
  NVML's `GpuInstancePlacement{Start, Size}` maps one-to-one onto the recorded
  `AcceleratorPlacement{Start, Length}`. Cost is roughly `1 + 2N` calls per MIG card per period with
  `N ≤ 7`, well under the existing per-device sampling.
- **Match by placement, not by process evidence.** Identifying the handle from "which instance has a
  process of ours" would report an *idle* partition as absent when the truth is zero.
- The known quirk is handled: on H100 with driver 570 `GetGpuInstance` answers `INVALID_ARGUMENT`, so
  the `GetGpuInstanceId` route is used instead.
- **The partition names and sizes itself off that same handle.** Its UUID becomes the entry's `id` and
  its own memory capacity becomes `memoryTotalMiB`, so the identity, the total and the usage all come
  from one read: half an answer would name a grant nobody can size. A read that failed leaves the
  entry under the parent device's identifier with no figures at all.
- Compute stays absent, reason `unsupported` (see Non-Goals).

### Verification

Per manufacturer, because the mechanism is uniform but the drivers are not. **A probe binary is the
primary vehicle** — it calls the vendor query plus attribution and prints the result, so a manufacturer
is verified without deploying anything; the Ascend host already runs a vLLM worker holding 57 GB, which
is live data to attribute. One **full end-to-end** run (dev image, sliced Instances, both surfaces read)
is the final check once F7 and F8 have landed. Hardware addresses stay out of this document.

| Manufacturer | Slicing kind | Where |
|---|---|---|
| NVIDIA | logical | an available multi-GPU host, first |
| NVIDIA | logical + **physical (MIG)** | a provisioned H100 cluster, as the later re-verification and F10's gate |
| T-Head | logical | the PPU host |
| AMD | logical | the two-card RDNA host |
| Ascend | logical | the eight-NPU host — also where F6 is validated |
| Iluvatar, Cambricon, Metax | logical | **no hardware.** Code only, carried by unit tests over recorded vendor payloads; shipped as unverified-on-hardware and said so in the docs |
| MThreads | — | not covered (see Non-Goals) |

Each hardware case asserts the same three things: a slice's reported memory tracks a workload's real
allocation as it grows; the reported figure is the slice's and not the card's, shown by running a
second Instance on the same card; and a driver that cannot answer leaves the field absent with the F5
gauge explaining it.

### Notes / Constraints / Caveats

- **There is no `coresTotalPercent`.** `coresUtilizationPercent` is already against the Instance's own
  allowance, so its total is 100 by construction and a field stating it would carry no information.
  The memory pair keeps a stated total because its unit is absolute.
- **`memoryUtilizationPercent` is stated rather than left to the consumer**, and is computed from the
  two figures published beside it, so the percentage and the pair can never disagree. It is absent
  whenever either of them is.
- **`memoryUsedMiB` may exceed `memoryTotalMiB` and is not clamped.** Clamping would present every
  leaking quota as a perfectly enforced one. But the overshoot is an *anomaly*, not proof: the units
  conversion floors, and driver overhead, partial attribution or duplicate samples can also produce
  one. The existing API doc promises Total/Used pairs suit percentage computation
  (`api/worker/v1/instance.metrics.go:31-41`); this pair's range therefore needs its own stated
  contract on the reference page.
- **`mode` carries the mode's NAME, not `workercore.DeviceAllocationMode`.** Reusing the enum was the
  first design and it was wrong on the wire: the type is a `uint32` with a `String()` and no
  `MarshalJSON`, so the subresource served `"mode": 1` while the Device Manager's exporter served
  `mode="Exclusive"` for the same grant — one field with two spellings, which is what this whole
  change exists to remove. A reader of a metric can do nothing with `3`. So the field is a `string`
  holding `String()`'s output, and the two surfaces are literally identical. It is retyped rather
  than given a marshaller on the enum, because `AcceleratorAllocation.Mode` is a STORED field of a
  served CRD and its wire format must not move; `InstanceMetrics` is computed per request, never
  stored, and its protobuf tag 9 was never released.
- **The vendor bindings' per-process calls are package-private** (`nvmlDeviceGetComputeRunningProcesses_v3`
  is lowercase; `binding/nvml/library_device.go` and `binding/ixml/library_device.go` expose no process
  method), so each binding needs a hand-written public device method. `library_device.go` is not
  generated, so that costs no regeneration — only Ascend's `.def` change does.
- `InstanceMetrics` exists only in `api/worker/v1` with no `v1alpha1` counterpart, so no conversion is
  involved.
- Vendor libraries are not trusted: a driver can return a garbage count, and a figure the card's own
  capacity contradicts is refused rather than published or clamped.
- Our HAMi-core pin (`pack/gpustack-operator/Dockerfile:17`) lags upstream's lock-recovery fix, so a
  SIGKILL in the wrong window silently removes the NVIDIA compute cap for a container's life. Bumping
  it is a separate one-`ARG` action, recorded here only because a slice whose cap has vanished will
  publish compute figures corresponding to no enforced limit.
- Go names stay snake_case per file, and every per-vendor adapter is a small function over a binding
  call so it tests without hardware.
- **Measured on accelerator hosts by T1's spike, not assumed:**
  - The vendor library reports **host** PIDs. On an NVIDIA host a containerised workload holding 256
    MiB reported itself as PID 1 inside its own namespace while NVML named its host PID, against a
    **non-empty** sample (708 MiB, the tensor plus the CUDA context).
  - **No `/proc` mount is needed, and adding one would not help.** `hostPID: true` already makes the
    Device Manager's own `/proc` list host processes
    (`deploy/gpustack-operator/chart/templates/device-manager/daemonset.yaml:51-52`, where `hostIPC`
    is set too). A `/proc/<pid>/cgroup` path is relativised against the **reader's cgroup
    namespace**, not against the procfs mount it is read through: a host `/proc` bind-mounted beside
    the container's own returned the byte-identical relative path.
  - **A Pod UID has two spellings and both are real.** An ordinary Pod's is a UUID, which the systemd
    driver escapes with underscores. A **static Pod's is 32 unseparated hexadecimal digits** — kubelet
    derives it from the Pod's name — and `crictl` reports that string as the Pod UID, so it must be
    accepted verbatim. Six such Pods were live on one control-plane node. Refusing them would not be
    a safe refusal: it converts a Pod that is excludable into one that makes its whole device
    unmeasurable.
  - **Both cgroup versions are in scope for the supported Kubernetes floor.** cgroup v2 was alpha in
    1.18, beta in 1.22 and GA in 1.25, and v1 is deprecated since 1.31 but not removed, so a v1.23
    floor spans both. Both were observed: a live v1 node wrote absolute paths across seven controller
    hierarchies, and the Device Manager read them **byte-identically to the host** because containerd
    leaves a v1 node in the host cgroup namespace.
  - **On cgroup v2 the container runtime gives each Pod a private cgroup namespace**, and the kernel
    then writes another Pod's path relative to the reader's root, collapsing everything above their
    common ancestor into `..` entries. The Pod segment survives that collapse for any Pod other than
    the reader's own, so attribution is unaffected; the `..` entries name nothing and are ignored.

### Boundaries

- **Always:** keep one field name meaning one thing in every mode; keep one implementation behind both
  surfaces within each surface's coverable scope; make a device's whole sample absent when any of its
  rows is unattributable; keep a vendored-header refresh additive.
- **Ask first:** anything touching the `hostIPC` default, `/dev/shm`, the Ascend `virtual-npu-id`
  allocation, `validateAdditionalVolumes`, the HAMi-core pin, or the NetworkPolicy default.
- **Never:** report a carved share's usage from a whole-card figure; report an unmeasured figure as
  `0`; publish a partial sum as a complete one; guess a container when the join fails; clamp away a
  measured overshoot; make a consumer branch on the mode to find its own number.

### Risks and Mitigations

- **Attribution is unproven in this repository** → T1 is the first task and gates the rest; if the
  cgroup mapping proves unreliable on a target runtime, the fallback is quota-only — the grant stated
  and every measurement absent, which is strictly today's behaviour plus F2.
- **The vendor PID namespace is an empirical question, not an assumption** → answered by T1's spike
  under the discipline `csrc/thead/ppu-slicing-shim/testing/hgml_util_probe.c:15-27` already applies
  for T-Head: a call that succeeds with zero samples is reported as `empty`, never as support, because
  that is the exact shape a false PASS would take. The first attempt reported exactly that `empty`, and
  the answer was only taken from a run with a real allocation behind it. The PIDs are host PIDs.
- **A process in the Device Manager's own Pod is unattributable on a cgroup v2 node** → its Pod segment
  sits above the reader's cgroup namespace root and is elided, so it is refused rather than inferred.
  Accepted knowingly: nothing in that Pod holds an accelerator, since initialising a vendor management
  library creates no device context and so puts no process in a device's list. If that ever changes,
  the namespace-immune source is the cgroup filesystem itself — `/sys` is already mounted from the
  host — at the cost of a scan instead of a single read.
- **PID reuse can attribute a row to the wrong Pod** → the identity is re-read after the vendor query
  and any change rejects the row; the residual window is documented, not claimed closed.
- **A whole GPU generation cannot answer the per-process query** → F5 makes the absence explicable.
- ~~The Ascend header refresh changes an existing generated signature~~ → **moot**: no refresh is
  needed, so the risk is designed out rather than mitigated. The diff of `make generate binding` is
  still the gate, and it caught a different hazard — that command regenerates *every* binding and
  drifts three of them on an address-valued constant and one on a cgo build constraint, so only the
  target binding's files are carried back.
- **Ascend's process count is an output-only parameter, so the library can write past the buffer** →
  contained, not merely documented: the read over-allocates a guard tail, a written guard row makes
  the read incomplete rather than short, and any overrun lands in memory the binding owns. Probed on
  hardware rather than inferred from the header, which says nothing about it.
- **Three manufacturers ship untested on hardware** → their adapters are pure functions over recorded
  payloads with unit tests, and the docs name which manufacturers are hardware-verified.
- **The snapshot's exposure grows** → documented on the reference page with a NetworkPolicy
  recommendation; the aggregate keeps raw PIDs off the wire.
- **Version skew reads as unsupported hardware** → the snapshot's schema version distinguishes them.
- **The aggregate outgrows the worker's 4 MiB read limit** → the section is bounded and the bound is
  asserted in a test.
- **A false idle reading while a Pod is being replaced** → the `timestamp` plus the Instance's own
  `status.phase`, which the console already holds, distinguishes it.

## Design Details

### Commands

Build, lint and test run **locally on darwin** — the whole module including the cgo vendor detectors
and `binding/dcmi` builds there (measured: 16 s cold for the affected trees).

```bash
go build ./pkg/... ./api/... ./binding/...
make lint                     # golangci-lint over the whole module; a --fix pass, cold runs are slow
make test                     # go test -v -failfast -race -cover -timeout=30m ./...
go test ./pkg/devicemanager/procattr/... ./pkg/devicemanager/detector/... \
        ./pkg/devicemanager/exporter/... ./pkg/worker/extensionapis/worker/... ./pkg/kubemetrics/...
make lint docs
```

**Code generation runs in the main checkout, not this worktree.** `make generate` derives package
paths GOPATH-style and requires a working directory ending in `gpustack.ai/gpustack`; this tree does
not. So for T3 and T4: apply the source edit in the main checkout, run the generator there, and sync
the resulting delta back. When syncing with `rsync`, use `--filter='P .git'` and **not**
`--exclude '.git/'` — a worktree's `.git` is a *file*, which the latter misses, and combined with
`--delete` it destroys the receiver's repository.

```bash
make generate                 # T4 — from the main checkout
make generate binding         # T3 — from the main checkout
```

**Hardware verification is remote over SSH**, one host per manufacturer, addresses supplied at run
time and never written here. A small probe binary is the primary vehicle; one full end-to-end run is
the final check.

### Project Structure

```
api/worker/v1/instance.metrics.go             # InstanceAcceleratorSliceMetrics
binding/*/library_device.go                    # hand-written public per-process device methods
binding/dcmi/dcmi_wrapper_api.def              # + dcmi_get_device_resource_info
binding/dcmi/dcmi_interface_api.h              # refreshed additively from a supported driver release
pkg/devicemanager/procattr/                    # host PID -> (pod UID, container); pure parser + /proc reader
pkg/devicemanager/detector/
  slice.go                                     # the monitor-loop pass; the snapshot's slice section
  <vendor>/process.go                          # one per-process adapter per manufacturer
  nvidia/mig_process.go                         # partition handle resolution (F10)
pkg/devicemanager/exporter/collector.go        # the mode label + capability gauge + the gate
pkg/kubemetrics/
  sample.go                                    # totals from spec.resources when there is no Pod
  accelerator.go                               # the grants index; one entry, resolved once
pkg/worker/extensionapis/worker/
  instance.metrics.go                          # the gate, and the entry resolution
docs/reference/instance-metrics.md
```

### Code Style

The reshaped API type, following the file's discipline — a figure derived from the allocation is
stated whenever the allocation can state it, and a measurement is a pointer that stays absent when
nothing could measure it:

```go
// InstanceAcceleratorMetrics is one accelerator an Instance holds, reported in the Instance's OWN
// terms: what it was granted on that accelerator, and what it was measured using of the grant.
//
// EVERY MODE REPORTS THE SAME FIELDS WITH THE SAME MEANING. An Instance holding a whole device reads
// the device's figures because the device is the grant; one holding a carved share — a logical slice
// or a hardware partition — reads that share's quota and that share's usage. So a consumer asking
// "how much memory is this Instance using, and how close is it to its ceiling" reads the same two
// fields whatever the allocation did, and never has to know. Mode says how the grant was made, for a
// consumer that wants to group by it; nothing else changes shape with it.
type InstanceAcceleratorMetrics struct {
	// ID is the universally unique identifier of the accelerator the Instance holds. Under
	// Partitioned this is the PARTITION's own identifier — a MIG UUID rather than the parent card's.
	ID string `json:"id" protobuf:"bytes,1,opt,name=id"`

	// Mode is how this accelerator was granted to the Instance. It does not change what the figures
	// below mean — it names the mechanism behind them. It carries the mode's NAME, spelled the same
	// way as the exporter's own `mode` label.
	Mode string `json:"mode" protobuf:"bytes,9,opt,name=mode"`

	// MemoryTotalMiB is the memory the Instance was granted on this accelerator, in MiB: the device's
	// own, a logical slice's quota, or a hardware partition's own capacity.
	MemoryTotalMiB *uint64 `json:"memoryTotalMiB,omitempty" protobuf:"varint,2,opt,name=memoryTotalMiB"`

	// MemoryUsedMiB is the memory the Instance was measured holding of that grant, in MiB. It MAY
	// EXCEED MemoryTotalMiB and is not clamped: clamping would present a leaking quota as a
	// perfectly enforced one.
	MemoryUsedMiB *uint64 `json:"memoryUsedMiB,omitempty" protobuf:"varint,3,opt,name=memoryUsedMiB"`

	// MemoryUtilizationPercent is MemoryUsedMiB over MemoryTotalMiB, stated so the percentage and
	// the pair it comes from can never disagree, and absent whenever either of them is.
	MemoryUtilizationPercent *uint32 `json:"memoryUtilizationPercent,omitempty" protobuf:"varint,4,opt,name=memoryUtilizationPercent"`

	// CoresUtilizationPercent is how much of the Instance's OWN compute allowance it was measured
	// using. A slice capped at a fifth of a card and saturating that fifth reads 100, not 20. It may
	// exceed 100 where a cap is not being enforced, and is coarse under a small cap because the
	// manufacturers measure the card in whole percent. Absent for a hardware partition.
	CoresUtilizationPercent *uint32 `json:"coresUtilizationPercent,omitempty" protobuf:"varint,5,opt,name=coresUtilizationPercent"` // nolint: lll

	// TemperatureCelsius, PowerUsageWatts and Unhealthy are WHOLE-DEVICE in every mode: a carved
	// share has none of its own.
	TemperatureCelsius *uint32 `json:"temperatureCelsius,omitempty" protobuf:"varint,6,opt,name=temperatureCelsius"`
	PowerUsageWatts    *uint32 `json:"powerUsageWatts,omitempty" protobuf:"varint,7,opt,name=powerUsageWatts"`
	Unhealthy          *bool   `json:"unhealthy,omitempty" protobuf:"varint,8,opt,name=unhealthy"`
}
```

Conventions: a doc comment states behaviour and the reason for it rather than restating the field name;
the absent-versus-zero rule is stated once per field that has it; percentages name their denominator; a
deliberately un-clamped field says why.

### Implementation Plan

Five foundations land in parallel, one producer task joins them, then the rest build over it.
Checkpoints: after T1–T5 (nothing user-visible yet, everything compiles and tests); after T6 (the
snapshot shows a slice's own figures on real hardware); after T7–T9 (every manufacturer, and the
partitions); after T10 (both surfaces serve the expression); after T11.

**The fan-out is narrower than it first looks, and the correction is worth stating.** Only the
per-manufacturer adapters (T7, T8) and the partition reader (T9) are genuinely independent of the
consumers: each owns producer files and touches nothing a surface reads. Everything a surface
publishes is **one** task (T10), because reshaping the API type moves every consumer of it in the same
change — a commit that reshapes the type and leaves them behind does not build. An earlier draft of
this plan split it into a subresource task and an exporter task and had to discover that edge; drawing
them apart mistook "reads the producer's output" for "can land without the other".

- [x] **T1 · Host-PID attribution, and the PID-namespace spike**
      Blocked by: None
      Owns: `pkg/devicemanager/procattr/**`
      Gate: review
      Acceptance: a pure parser resolves a cgroup line set to (pod UID, container ID) for cgroup v1 and
      v2 under both the systemd and cgroupfs drivers, and **rejects rather than guesses** every
      ambiguous shape in the Test Plan's attribution table; a thin `/proc` reader supplies the lines
      and reports `ENOENT`/`EACCES` as distinct reasons. On one accelerator host, the vendor library's
      reported PIDs are shown to be host PIDs — or shown not to be — by the discipline
      `csrc/thead/ppu-slicing-shim/testing/hgml_util_probe.c:15-27` uses: a call that succeeds with
      zero samples is reported `empty`, never as support.
      Verify: `go test ./pkg/devicemanager/procattr/...` plus the recorded probe output from one host.

- [x] **T2 · Public per-process wrappers in the vendor bindings**
      Blocked by: None
      Owns: `binding/nvml/library_device.go`, `binding/ixml/library_device.go`,
      `binding/amdsmi/library_device.go`, `binding/rsmi/library_device.go`,
      `binding/hgml/library_device.go`, `binding/cndev/library_device.go`,
      `binding/mxsml/library_device.go`
      Gate: review
      Acceptance: each binding exposes a public device method returning that vendor's per-process rows
      in the vendor's **native units**, surfacing rather than flattening the vendor's own semantics —
      NVML's `NVML_VALUE_NOT_AVAILABLE` memory sentinel and its non-zero-only, multi-timestamp
      utilization samples; a buffer-resize retry that reports exhaustion instead of returning a
      partial buffer; a hard ceiling on the row count so a garbage count is refused rather than
      allocated. No generated file changes.
      **AMD SMI's one-second minimum makes one library call per invocation the only honest shape**:
      the vendor requires the gap "before every following call" while mandating a probe-then-fill
      pair, so the probe and the fill are split across monitor ticks with the caller supplying the
      capacity — documenting the constraint cannot satisfy it, because both calls are inside the
      method. **Those AMD SMI wrappers ended up with no caller**, for the reason T9 records: the
      membership query they exist beside answers `INVAL` on hardware, so the adapter reads ROCm SMI
      instead. They stay as a faithful binding of the vendor's API, documented as unusable on the
      release measured.
      One further correction the hardware made, after this task was checked off: **a symbol that
      resolves is not a symbol that works.** T-Head's versioned process-utilization entry point is
      exported and answers `NOT_SUPPORTED` to every call, while the older one beside it serves the
      query — so a binding that prefers the newer symbol and reports its refusal blames the hardware
      for a stub in the library. The two T-Head paths now fall back on a refusal of the entry point
      itself, and never on a transient failure. NVML was checked for the same shape on driver
      610.43.02 and does not have it: both of its symbols are real implementations, so its preference
      order is unchanged rather than churned on another vendor's defect.
      Verify: `go build ./binding/...`, then `go vet` over the seven owned packages. **Not**
      `go vet ./binding/...`: that fails on `binding/hsa/cgo_helpers_static.go:106` with
      `possible misuse of unsafe.Pointer`, in a package this task does not touch and reproduced at a
      pristine checkout, so the whole-tree form can never pass and would mask a real regression by
      being red either way. `make lint` is green over the same tree and is the project's own gate.

- [x] **T3 · Ascend binding: wrap the per-process call**
      Blocked by: None
      Owns: `binding/dcmi/**`, `gen/binding/dcmi/**`
      Gate: review
      Acceptance: `dcmi_get_device_resource_info` has a public Go entry point that treats the
      library's output-only count as the hazard it is — a guard tail, and a truncated read reported as
      incomplete rather than short. **No header refresh**: the function and its struct are already
      declared identically to the host's, so a refresh buys nothing and risks the one thing this task
      forbids. Whether a per-vNPU query is reachable is determined and reported.
      Verify: `make generate binding` from a module-suffixed physical path, then
      `git diff --stat binding/dcmi` shows additions only, and `go build ./binding/dcmi/...`.
      **Carry back only `binding/dcmi`**: that command regenerates every binding and introduces two
      kinds of unrelated drift — an address-valued constant that differs per run in `amdsmi`, `cndev`
      and `rsmi`, and a dropped `linux` build constraint on a cgo `LDFLAGS` line in `ixml`, which
      would apply linux-only linker flags on darwin.

- [x] **T4 · Totals without a Pod**
      Blocked by: None
      Owns: `pkg/kubemetrics/sample.go`, `pkg/worker/controllers/worker/instance_test.go`
      Acceptance: a sample built from an Instance's `spec.resources` carries the same three totals as
      one built from the Pod the controller renders for that same spec, asserted side by side; no
      measured field is populated.
      Verify: `go test ./pkg/kubemetrics/... ./pkg/worker/controllers/worker/...`

- [x] **T5 · The started-container predicate, decided once**
      Blocked by: None
      Owns: `pkg/kubemetrics/pod.go`
      It is one task rather than a clause in each of the two consumer tasks because both surfaces have
      to decide it **identically**, and `pkg/kubemetrics` is already "the single implementation behind
      the two surfaces reporting these figures — so the two can never drift apart by a unit or a
      rounding step". Two surfaces deciding the same question separately is exactly what this spec
      exists to prevent.
      Acceptance: the predicate answers on the Pod's container **statuses** across all three container
      lists, and reports "started", never "ready"; the unstarted sample carries the declared totals and
      an explicit zero for each usage, so a consumer reads a measurement rather than an absence.
      Verify: `go test ./pkg/kubemetrics/...`

- [x] **T6 · Producer: the snapshot slice section, attribution, aggregation, NVIDIA logical adapter**
      **Outstanding: the on-hardware snapshot assertion.** The code and its unit coverage are done and
      the section is queried only for devices carrying a carved allocation, but the acceptance's live
      check — a sliced Instance reading its own figure and not the card's, with a second Instance on
      the same card proving it — needs a cluster with NVIDIA accelerators and a deployed dev image.
      It runs with the end-to-end pass rather than here, and is listed there.
      Blocked by: T1, T2
      Owns: `pkg/device/types.go`, `pkg/devicemanager/detector/slice.go`,
      `pkg/devicemanager/detector/detector.go`, `pkg/devicemanager/detector/nvidia/process.go`
      Gate: review
      Acceptance: `MonitorSnapshot` carries a dedicated slice section keyed by (pod UID, container,
      device ID) with a schema/feature version and the six diagnostic counts; raw per-process rows are
      stripped before the snapshot is stored; an unattributable row makes **every** slice figure on
      that device absent; the section's size is bounded and the bound asserted; on the NVIDIA host
      `curl -sk https://127.0.0.1:32443/monitor/snapshot` shows a sliced Instance's own memory figure
      and not the card's, with a second Instance on the same card proving it.
      Verify: `go test ./pkg/devicemanager/detector/... ./pkg/device/...` plus the recorded snapshot.

- [x] **T7 · The four hardware-verified adapters**
      Blocked by: T2, T3, T6
      Owns: `pkg/devicemanager/detector/{nvidia,amd,thead,ascend}/process.go`,
      `pkg/devicemanager/detector/amd/device.go`
      One task rather than four: they are the same shape over four bindings, and each one's acceptance
      is a run on its own host, so splitting them would buy parallelism the hosts do not have.
      Acceptance: on each host, a sliced Instance's reported memory tracks its workload's real
      allocation as it grows and reads the slice's figure rather than the card's when a second Instance
      shares the card. T-Head contributes only the newest utilization sample per PID, matching the
      shim's own compute controller. Ascend is **memory only** — T3 established on hardware that no
      per-vNPU query exists to call — and sums a container's PIDs **per device**, never across them,
      which is what the live eight-NPU vLLM worker required: one host PID per chip in a single
      container.
      **THIS ADAPTER READS ROCm SMI, NOT AMD SMI, AND THE HARDWARE DECIDED IT.** The plan above was
      built on AMD SMI: one library call per invocation with the buffer capacity carried across monitor
      ticks, because that library's process list is only valid a second after the previous call, and a
      membership query to tell which cards a pid actually holds — since AMD SMI's own per-device
      process list ignores the processor handle it is given and answers with every compute process the
      driver knows. On the host, that membership entry point
      (`amdsmi_get_gpu_compute_process_gpus`) answers `INVAL` for a live process id, with the
      documented count-probe shape and with a pre-sized buffer alike, though the symbol resolves and
      its signature matches both that release's header and AMD's published documentation. Without
      membership no row can be told from another card's, so every figure went absent.
      ROCm SMI answers the same relation PER DEVICE, which is both the figure this feature wants and
      the route `rocm-smi` itself takes on that stack. So the adapter reads it there, and what it gets
      is better than AMD SMI could have given even working: the process's memory ON that card rather
      than the process-wide total AMD SMI's own header warns is not device memory, plus a compute
      figure where the GFX revision measures occupancy. Three consequences replace the plan's:
      the buffer-capacity ledger and the one-extra-sampling-period gap are **gone** — a figure arrives
      on the first monitor tick; ROCm SMI is initialized **before** AMD SMI, because loading them the
      other way round aborts the process with SIGBUS inside `dlopen`; and the AMD SMI wrappers T2 added
      for this stay in the binding, unused and documented as answering `INVAL`, because they are a
      faithful binding of the vendor's API and a later release may implement it.
      Verify: `go test ./pkg/devicemanager/detector/{nvidia,amd,thead,ascend}/...` plus the recorded
      probe output from each host.

- [x] **T8 · The four adapters with no hardware**
      Blocked by: T2, T6
      Owns: `pkg/devicemanager/detector/hygon/process.go`,
      `pkg/devicemanager/detector/iluvatar/process.go`,
      `pkg/devicemanager/detector/cambricon/process.go`,
      `pkg/devicemanager/detector/metax/process.go`
      Acceptance: each converts recorded vendor payloads to the section's shape under unit test;
      Cambricon additionally carries per-process utilization; **none claims a compute figure its
      binding does not expose** (Iluvatar in particular); each is marked unverified-on-hardware.
      Verify: `go test ./pkg/devicemanager/detector/{hygon,iluvatar,cambricon,metax}/...`

- [x] **T9 · Hardware partitions: instance handle resolution, identity and memory**
      Blocked by: T6
      Owns: `pkg/devicemanager/detector/nvidia/mig_process.go`
      Gate: review
      Acceptance: a partition allocation's device handle is resolved by matching the recorded profile
      and placement against the enumerated instances — **not** by which instance has one of our
      processes, so an **idle partition reports zero rather than absent**; per-instance memory is read
      on that handle; compute utilization stays absent with reason `unsupported`; the `GetGpuInstance`
      `INVALID_ARGUMENT` behaviour on H100/driver-570 is handled via `GetGpuInstanceId`.
      **The partition names and sizes itself off that same handle**: its UUID becomes the entry's `id`
      and its own capacity becomes `memoryTotalMiB`. Both come from the driver rather than from the
      allocation, because both would otherwise be wrong — a `1g.10gb` of an H100 carries 9856 MiB where
      the memory-anchored units say 10240, and the parent card's identifier names something the
      Instance was not granted. Identity, capacity and usage come from ONE read: half an answer would
      name a grant nobody can size, so a read that failed leaves the entry under the parent device's
      identifier with no figures at all.
      The reverse lookup costs **60–90 driver calls per MIG card per monitor period**, not the `1 + 2N`
      this spec first estimated: that omitted the profile catalogue walk needed to turn a recorded
      profile *name* into an id. At a 15-second period it is about five calls a second per card.
      Three rules were added while building it, each closing a way to publish a figure that is wrong
      rather than absent. **An incomplete profile catalogue names no cap.** The card's own compute-slice
      count is the largest the catalogue carries, so a catalogue the driver could not fully answer for
      can only understate the card — and a cap read against an understated card overstates every
      partition on it, reporting a 1g of an H100 whose 7g profile went unread at 25% instead of 14%. The
      memory figures are unaffected: they are read on a handle, not derived from that arithmetic.
      **A partition two containers claim is dropped for both.** One partition has one tenant, so two
      records naming it are records at least one of which is wrong and nothing on the node can tell
      which; it is the same refusal the consumer's join already makes for a device two containers of one
      Pod carved, made in the producer because only the producer sees across Pods. **An instance id two
      MIG handles claim stays unaddressable however many more claim it** — dropping the map entry alone
      let a third handle re-register the very id the second one disqualified.
      **The section carries a schema version and the consumer accepts a range**, though only version 1
      exists so far. The producer and both consumers live in separate Pods upgraded at separate times,
      so a new consumer reads an old device manager during every rollout: refusing a whole section
      would throw away the figures it does serve in order to report a record kind it never wrote. A
      version this build knows is read as what it is; a version outside the range answers nothing at
      all, with reason `version_skew`.
      Verify: `go test ./pkg/devicemanager/detector/nvidia/...` plus the recorded H100 output.

- [x] **T10 · The expression: one accelerator entry, on both surfaces**
      Blocked by: T4, T5, T6, T9
      Owns: `api/worker/v1/**`, `api/worker/zz_generated.openapi.go`,
      `pkg/kubemetrics/accelerator.go`, `pkg/worker/extensionapis/worker/**`,
      `pkg/devicemanager/exporter/**`
      One task, not three, and the shape of the type is why. Reshaping `InstanceAcceleratorMetrics` and
      moving both surfaces onto it is a single change: every consumer of the type has to move with it,
      so an API commit that leaves them behind does not build. The resolution itself lives in
      `pkg/kubemetrics` for the reason T5's predicate does — two surfaces resolving the same grant
      separately would agree today and drift on the case neither exercises.
      Gate: review
      Acceptance: an `accelerators[]` entry reports the Instance's own grant and its own usage in every
      mode, and **no card figure it did not measure**; a hardware partition is keyed by its own
      identifier and sized by its own capacity; a slice's measured share of the card is restated
      against the cap the allocator enforced, rounded up and unclamped; the exporter's seven
      accelerator families carry a `mode` label and no new family exists; a `Visibility` sidecar is not
      read as a second claimant on its sibling's accelerator; an Instance that has started nothing
      answers `200` on one surface and publishes nothing on the other, with **zero** upstream requests
      (a fake fails the test if called); `api_exporter_same_fixture` asserts the two surfaces agree on
      identity, mode, values and every omission decision.
      Verify: from the main checkout `make generate`, sync back, then `make generate &&
      git diff --exit-code`; `go test ./api/... ./pkg/kubemetrics/...
      ./pkg/worker/extensionapis/worker/... ./pkg/devicemanager/exporter/...`

- [x] **T11 · Documentation**
      Blocked by: T7, T8, T9, T10
      Owns: `docs/**`, `README.md`, `.claude/skills/gpustack-operator-e2e/**`
      Acceptance: the reference page states what each field means in every mode, the availability
      matrix naming which manufacturers are hardware-verified, the capability probe and its reasons,
      the started-container gate replacing the three `503`s, the exporter's Pod-less gap, the
      un-clamped pair's contract, and the snapshot exposure note with the NetworkPolicy recommendation.
      Two additions the build earned. The page answers **"which metric is this Instance's own usage"**
      in one line — one metric name, in every mode — and says plainly that `mode` is there to group and
      filter by rather than to choose a metric with, because that is the property the whole expression
      exists to give. And the availability matrix gained a **driver-dependent** mark, because two
      backends were measured refusing a compute call their headers declare.
      `Owns:` widened past `docs/**` by the README's capability line and the two e2e cases the load
      simulation is folded into — see the Test Plan's e2e section for why those cases and not new ones.
      Verify: `make lint docs`

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make
this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- `pkg/worker/extensionapis/worker` needs a fake for the three upstream sources (kubelet stats,
  `metrics.k8s.io`, the Device Manager fetch) that **fails the test when called**, so F1's
  zero-upstream-I/O claim is asserted rather than assumed. No such fake exists today.
- `pkg/devicemanager/detector` needs a `device.Detector` fake returning recorded per-process rows, so
  the aggregation and the absent-vs-zero rules test without hardware.
- `pkg/devicemanager/exporter` needs its collector fixture extended with a slice section, and a
  shared fixture with the subresource so `api_exporter_same_fixture` can assert byte-equal decisions.
- Recorded vendor payloads per manufacturer (one file each) are the substitute for hardware on
  Iluvatar, Cambricon and Metax; they are captured from the probe binary on the hosts we do have and
  hand-written for the three we do not.

#### Unit tests

Current coverage, measured 2026-08-13 with `go test -cover`:

- `pkg/kubemetrics`: `2026-08-13` - `99.4%`
- `pkg/devicemanager/exporter`: `2026-08-13` - `98.3%`
- `pkg/device`: `2026-08-13` - `82.2%`
- `pkg/devicemanager/detector`: `2026-08-13` - `50.7%`
- `pkg/devicemanager/detector/thead`: `2026-08-13` - `35.1%`
- `pkg/devicemanager/detector/nvidia`: `2026-08-13` - `22.5%`
- `pkg/worker/extensionapis/worker`: `2026-08-13` - `20.1%`
- `pkg/devicemanager/detector/ascend`: `2026-08-13` - `6.1%`
- `pkg/devicemanager/procattr`: new package, no baseline

Targets: the new packages carry table-driven coverage of every case below and must not regress the
package they live beside. `pkg/kubemetrics` and `pkg/devicemanager/exporter` must stay above 95%.

**Attribution cases** (`pkg/devicemanager/procattr`) — fixture pod UID
`01234567-89ab-cdef-0123-456789abcdef`, container ID `deadbeef`. Every "reject" case is the point: a
wrong-but-plausible attribution is far worse than none.

| Case | Fixture | Expected |
|---|---|---|
| `v2_systemd_containerd` | `0::/kubepods.slice/…-pod01234567_89ab_….slice/cri-containerd-deadbeef.scope` | exact pod UID + container |
| `v2_systemd_crio` | same unit with `crio-deadbeef.scope` | exact pod UID + container |
| `v2_cgroupfs_containerd` | `0::/kubepods/burstable/pod01234567-…/deadbeef` | exact pod UID + container |
| `v1_cgroupfs_multiple_controllers` | cpu and memory lines carry the same pod/container | one attribution, not double-counted |
| `v1_systemd_underscore_uid` | escaped systemd UID across controllers | canonical hyphenated UID |
| `same_uid_all_v1_controllers` | every controller names the same pod | accept once |
| `thread_id_same_domain_cgroup` | a TID whose cgroup matches its group leader | same pod + container |
| `threaded_cgroup_child` | TID in a nested threaded cgroup under the pod | accept only if exactly one pod ancestor and the container stays unambiguous |
| `known_non_instance_pod` | valid pod UID absent from the Instance-pod index | classified known-non-Instance, excluded safely |
| `sibling_instance_same_card` | two PIDs, two known Instance pods | each attributed to its own pod only |
| `host_process_no_pod_component` | `/system.slice/nvidia-persistenced.service` | unattributed → the card's sample is incomplete |
| `unknown_pod_uid` | syntactically valid `pod<uid>` not in the index | unattributed, never assigned |
| `container_id_unknown` | known pod, scope matches no `ContainerStatuses` entry | no container-keyed result; never guess `main` |
| `pod_uid_substring` | segment is `worker-pod<uid>-cache` | reject |
| `uid_in_container_id` | container scope text happens to contain `pod<uid>` | reject; no substring matching |
| `two_different_pod_ancestors` | canonical components for two pod UIDs | ambiguous → reject |
| `v1_controllers_disagree` | cpu names pod A, memory names pod B | ambiguous → reject both |
| `pid_exited_before_read` | `ENOENT` on `/proc/<pid>/cgroup` | unattributed, reason distinct from permission |
| `pid_permission_denied` | `EACCES`/`EPERM` | unattributed with `permission`, not "idle" |
| `zombie_process` | `stat` state `Z`, cgroup still names the pod | reject as a stale vendor row |
| `pid_reused_by_sibling_pod` | the row was pod A's; `/proc/<pid>` now belongs to pod B | must reject; never attribute to B |
| `cgroup_changes_during_read` | two identity reads disagree | reject |
| `vendor_pid_namespace_hidden` | no matching host `/proc` entry | unavailable, `pid_namespace_invisible` |
| `kata_qemu_pid` | the row maps to a runtime VM process | reject unless a proven one-pod/one-device mapping exists |
| `gvisor_sandbox_shared_container_identity` | sandbox PID maps to a pod but not a unique container | no container-keyed attribution |
| `mps_daemon_in_host_cgroup` | a client pod uses the GPU through a host MPS daemon | unattributed; do not report the client as zero |
| `mps_daemon_in_sibling_pod` | a shared daemon in pod A serves A and B | reject as mediated; never charge all usage to A |
| `process_on_two_devices` | one PID appears in two device queries | attributed independently per device, no cross-device duplication |
| `sliced_container_two_allocations` | container maps fine but the allocation names two accelerators | one attribution per accelerator; the plural case is legal, not an error |

**Absent-versus-zero cases** (`pkg/devicemanager/detector`, `pkg/worker/extensionapis/worker`,
`pkg/devicemanager/exporter`) — `present(0)` is a non-nil pointer to zero; `absent` is omitted.

| Case | Condition | Expected |
|---|---|---|
| `supported_idle_no_processes` | complete query, no target processes | `present(0)`, series `0` |
| `supported_only_other_pods` | complete query, only other known pods | `present(0)` |
| `target_process_reports_zero` | the row reports zero explicitly | `present(0)` |
| `ready_and_idle` | started, complete query, no usage | all measurable fields `present(0)` |
| `unsupported_entrypoint` | `NOT_SUPPORTED` | absent; gauge reason `unsupported` |
| `permission_failure` | `NO_PERMISSION` | absent; reason `permission` |
| `transient_driver_error` | query fails after the probe succeeded | absent for this sample; the probe must not claim success |
| `unattributed_row_on_device` | one row unattributable | usage absent for **every** slice on that device |
| `known_non_instance_row` | row conclusively a non-Instance pod | excluded safely; target may still be `present(0)` |
| `one_of_two_target_rows_unavailable` | one valid row, one sentinel | the whole pod/device field absent; never a partial sum |
| `buffer_truncated` | driver reports more rows than the bound | absent, reason `truncated` |
| `resize_race_exhausted` | required count grows on every retry | absent, not the last partial buffer |
| `duplicate_pid_samples` | several utilization timestamps for one PID | only the newest valid sample contributes |
| `negative_or_reversed_delta` | the counter window moves backwards | absent, not zero |
| `old_device_manager_snapshot` | fresh snapshot, no slice section/version | totals still derived; measured fields absent, `version_skew` |
| `stale_snapshot` | older than three monitor periods | no measured field, no series; capability support does not imply freshness |
| `supported_memory_compute_unsupported` | memory answers, compute does not | memory present, compute absent, independent reasons |
| `no_started_container_active_gpu` | no container started | zeros, and **zero upstream calls** |
| `running_not_ready_active_gpu` | readiness false while the process is measured active | **measured, never zero** — the case F1's predicate exists for |
| `no_pod_stopped` | no backing Pod | subresource returns declared totals with zeros; the exporter publishes nothing and must not fabricate parity |
| `sub_mib_sum` | native-byte total non-zero but under 1 MiB | sum native, convert once, result stays non-zero |
| `used_exceeds_grant_below_card_total` | measured above the grant, below card capacity | publish unchanged |
| `cores_over_cap` | measured share above the enforced compute cap | restated above 100 and published; never clamped |
| `cores_small_cap_rounds_up` | a busy slice under a cap too small to divide evenly | rounded up; a measurably busy slice never reads as idle |
| `used_exceeds_card_total` | vendor sum above physical capacity | one explicit invalid-data rule; never silently zero |
| `container_carves_two_cards` | a sliced container owns more than one card | one entry per card, each measured on its own; nothing suppressed |
| `two_containers_one_card` | two containers of one Pod are granted the same device | that entry reports no figures of its own; never one picked nor the two summed |
| `visibility_sidecar` | a sidecar granted `Visibility` on its sibling's card | not a second claimant; the sibling's grant is reported normally |
| `idle_partition` | a MIG partition with no processes | `present(0)` — the reason F10 matches by placement |
| `partition_supersedes_per_process` | a partition record and a per-process record for one key | the partition's own handle answers; never a per-field mixture of the two |
| `partition_unread` | a partition whose handle could not be read | the parent device's identifier and **no figures at all** — never the card's, never a total folded out of the units |
| `partition_catalogue_incomplete` | the driver failed on a profile id it did not disclaim | the profiles it did answer for still match; nothing is derived from the catalogue's completeness |
| `partition_own_capacity` | a `1g.10gb` partition of an H100 | 9856 MiB, the driver's own — not the 10240 the name and the units both round to |
| `partition_identity` | a partition read successfully | the entry is keyed by the MIG UUID, not the parent card's |
| `partition_claimed_twice` | two containers record the same profile and placement on one card | nothing reported for either; a sibling partition on the same card is unaffected |
| `partition_placement_vacated` | the recorded placement matches no live instance | absent, never a sibling instance's figure |
| `schema_range_oldest_producer` | a section at the oldest version this build reads | read as what it is, rather than refused as unreadable |
| `nil_pointer_json` | measurement unavailable | JSON key omitted, Prometheus family absent |
| `zero_pointer_json` | measured and idle | JSON key present with `0`, sample present with `0` |
| `mixed_device_capability` | device A supports the entry point, B does not | two distinguishable device results; no duplicate label set |
| `producer_partial_snapshot` | card metrics succeed, process query fails | card fields remain; slice usage absent; the source is not reported wholly successful |
| `api_exporter_same_fixture` | identical Pod, allocation and snapshot to both surfaces | exact equality of identity, mode, values, optionality and omission decisions |

#### Integration tests

- The producer pass against a fake `device.Detector` and a fake `/proc` tree: a two-Instance node
  sharing one card, where each Instance's aggregate is its own; then the same tree with one host
  process added, where both Instances' figures go absent.
- The snapshot round trip: producer → JSON → the worker's decoder, asserting the aggregate survives,
  raw PIDs are absent from the wire, and a section larger than the bound is rejected rather than
  truncated.
- Version skew both ways: a worker against a section it cannot read (absent, `version_skew`) and one
  against a section carrying fields it does not know (ignored, no decode failure at the 4 MiB limit).
- Concrete test names are added after the implementation PR merges.

#### e2e tests

**Hardware acceptance, in one place.** A task is checked off when its code and its unit-testable
acceptance are met; the acceptance clauses that name a host are tracked here instead, so there is one
list to read rather than a note under each task.

**All five are now met against real drivers**, and each run is recorded below with the figure it
produced. Three of them found defects that unit tests could not: a cgo pointer-rule violation that made
every NVIDIA per-process utilization read panic, an AMD membership entry point that answers `INVAL` on a
live process id, and — on the deployed run, which is the only one that reads the served API rather than a
probe binary — the `mode` field arriving as the enum's wire value where the case asserted its name.

**Still owed, and tracked rather than carried here: the third bullet below**, which needs two
accelerators on one node and so could not run on the single-card cluster the deployed assertion used.
The final round of changes this spec's Alternatives records — `mode` as a name, a partition addressed by
its recorded identifier, one entry per grant, and the T-Head partition reader — was written after that
cluster was destroyed and has not been read off a driver either. Both are filed as `todo`
issues so neither is carried as a paragraph nobody owns.

| Task | Host | What the driver answered |
|---|---|---|
| T6 deployed | one H100 on a Kubernetes cluster, MIG off, the dev image rolled out | two 40% slices of ONE card, only one loaded: **518 MiB and 0 MiB**, each against its own **32623 MiB** quota — 40% of the card, not the card. The idle slice's zero is a measurement, not an absence: `/monitor/snapshot` carried `rowsReturned: 1, rowsAttributed: 1`, so the per-process table was read and held nothing of that slice's. Read off the snapshot and both consumer surfaces in one run, and they agreed field for field |
| T9 partitions | one H100, MIG on, three profiles carved | each partition named by its own MIG UUID and sized by its own handle; the busy `3g.40gb` read **8889 MiB** while its idle siblings read **28** and **14 MiB** — their own, neither the card's nor absent; a placement no instance occupies read absent with `transient_driver_error`; per-process utilization `NOT_SUPPORTED`, published as `unsupported` |
| T9 AMD | two-card RDNA3, gfx1101 | a 2 GiB HIP workload reported **2243 MiB against the card holding it and no row at all on the other**, matching `rocm-smi` to the byte; the idle card read a measured zero; `cu_occupancy` carried the invalidation sentinel on every row, so compute is absent while memory is not |
| T10 T-Head | 16 PPUs, live workload | the workload's **91339 MiB** on its own card, the other fifteen measured zero; compute **works** at `cu 0%` for the same pid — a measured zero on an idle vLLM, matching `ppu-smi pmon` — but only after the fallback below |
| T11 Ascend | eight 910B2, live vLLM | eight devices, **eight distinct pids, 57280 MiB each** — the tensor-parallel case this task exists for, one worker per chip, each attributed to its own chip and summed nowhere else; per-vNPU compute `unsupported`, as T3 established |

**A present symbol is not a working one, and the binding now proves it rather than assuming it.** On the
PPU driver measured, `hgmlDeviceGetProcessesUtilizationInfo` — the versioned symbol the binding
preferred because it resolves — answers `NOT_SUPPORTED` to every call, while the older
`hgmlDeviceGetProcessUtilization` beside it serves the query. That is the symbol the vendor's own
`ppu-smi pmon` binds, which is how the discrepancy was found: the tool showed a per-process `cu` column
for the very pid our adapter had just reported as unmeasurable. Preferring the newer symbol and
publishing its refusal blamed the hardware for a stub in the library, so the binding now falls back on a
refusal OF THE ENTRY POINT — `NOT_SUPPORTED`, a struct-version mismatch, or the API-unavailable family —
and never on a transient failure, which says nothing about the other symbol.

**The same shape was checked on NVIDIA and is not present there.** Both symbols exist on driver
610.43.02 and both are real implementations: the versioned count probe answers `INSUFFICIENT_SIZE` with
a required size, and the wrapper reads `sm=99%` for a process on a card `nvidia-smi` shows at 100% while
the idle neighbour answers `NOT_FOUND`, which the adapter reads as a measured idle. So `nvml` keeps its
preference order unchanged — a fallback there would have been a change to a path verified working, on
the strength of another vendor's defect.

The load was simulated where the hardware had no workload of its own. On the H100 a CUDA process held
8 GiB inside one MIG instance and spun matmuls; on the AMD host a HIP program held 2 GiB on one card
and kept a kernel resident. **`dcgmproftester` was tried first on the H100 and could not be used**: it
fails to initialize DCGM inside a MIG instance (`CacheManager Init Failed. Error: -17`,
`dcgmStartEmbedded() returned -7`), so a plain CUDA allocation is what the memory figure was read
against. The T-Head and Ascend hosts carried their own live workloads and needed no simulation.

The probe binary carries per-manufacturer verification during the fan-out; one full end-to-end run is
the final check, on a Kubernetes cluster with accelerators:

- Deploy the dev image, create a logically sliced Instance, and read both surfaces: the entry's
  `memoryTotalMiB` is the quota rather than the card, its `memoryUsedMiB` tracks the workload's real
  allocation, and `coresUtilizationPercent` is present where the manufacturer serves it.
- Run a second sliced Instance on the same card and assert each reads its own figure, not the card's —
  the single assertion that proves the whole feature, read off
  `curl -sk https://127.0.0.1:32443/monitor/snapshot` as well as off the two consumer surfaces, so a
  disagreement between the producer and either surface is caught in the same run. **Met** — the T6
  deployed row above carries the figures.
- Assert the producer queried only the carved devices: a whole-card Instance on a second accelerator
  in the same node yields no slice record for that accelerator, and a host process on it poisons
  nothing. **Not met** — this is the one that needs two accelerators on one node, and the deployed run
  had one. It is what proves the per-process pass is scoped to the carved cards rather than sweeping
  the node, so it is owed rather than optional.
- Create a hardware-partitioned Instance on the H100 cluster: the entry keyed by the MIG UUID, its
  total the partition's own capacity, compute absent with `unsupported`, and an idle partition reading
  `0` rather than absent. **Met at the producer** by T9's run above; what a deployment would add is
  the same figures read off the two consumer surfaces rather than off the adapter.
- Stop an Instance and confirm the subresource answers `200` with declared totals and zeros while the
  exporter publishes no series for it.
- Raise the Device Manager's verbosity at runtime (`PUT /debug/flags/v` on the secure port) and confirm
  the diagnostic counts explain an absent figure.

**The load simulation is landed in the e2e suite, folded into cases that already existed** rather than
as cases of its own — the fixtures it needs were already built there:

- **CASE 14** (two 40% slices on one card) carries the assertion this whole feature rests on. It is the
  only existing fixture with two slices of ONE card, which is what tells "its own share" apart from
  "the card's": a surface reporting the card would give both Instances the same number. A new step
  reads each Instance's `accelerators[]` entry and refuses any of `mode` ≠ `Sliced`, a
  `memoryTotalMiB` equal to the whole card's, or a `memoryUsedMiB` equal to the card's own.
  `E2E_SLICE_LOAD_IMAGE`
  gives slice A a workload that allocates device memory while B stays idle, and then A's figure must be
  non-zero and differ from B's. Unset, the case still asserts the structural half, so it stays runnable
  on a cluster with no such image.
- **CASE 37** (the metrics subresource) gains the started-container gate, which needs no accelerator at
  all: the Instance is stopped so it holds no Pod, and the same call must still answer with the declared
  totals and zero measurements rather than the `503` this surface used to return.
- The load recipe is the one this build verified — a CUDA allocation inside the slice — and the case
  header records why `dcgmproftester` is not a substitute: it cannot initialize DCGM inside a MIG
  instance, and on a logical slice it measures the whole card rather than the share.

## Alternatives

- **Read each slicing shim's usage region as the source.** Attractive because the numerator then
  shares an accounting basis with the quota and no PID attribution is needed. Rejected: it needs one
  binary-layout reader per backend where one binding call serves all; two of the four regions are not
  ours, and the upstream NVIDIA one carries neither a timestamp nor a measured flag, so a zero compute
  figure cannot be told from "never sampled"; and Ascend's carries no memory figure at all. It was
  also carried for a while as a *second*, quota-basis figure beside the measured one, for the two
  backends whose shim is ours — dropped because a field present on two of eight manufacturers, whose
  value is the disagreement with the field beside it, is an operator's diagnostic rather than part of
  a tenant's own view, and this expression is the tenant's own view.
- **A `slice` block beside the device's own figures.** The shape this spec was first built to, and
  rejected on review of the built thing: it made the metric a consumer reads depend on how the
  Instance holds the card, so every console and every dashboard needed a branch or an `or` fallback,
  and reading the wrong one gave a plausible number that was somebody else's. Collapsing it means the
  card-wide view is no longer on the per-Instance surface at all, which is the deliberate cost: that
  view belongs to a node- or device-scoped surface.
- **Carry the per-process rows inside `device.AcceleratorMetrics` on the wire.** Rejected: that type is
  one card's readings, and making it a tenant transport both widens what an unauthenticated endpoint
  exposes and mismatches the (pod, container, device) join the consumers need. Raw rows stay
  producer-internal; the snapshot carries a dedicated aggregate section.
- **Add a method to the `device.Detector` interface.** Rejected: it changes nine implementations and
  every test fake at compile time, buys no cross-version benefit (producer and implementation are one
  binary), and a second call would enumerate the same devices twice, yielding non-atomic card and
  process samples.
- **Add an `unavailableReason` field to the API.** Considered and not taken: the reason is served by the
  exporter's capability gauge and the Device Manager log, and the subresource stays minimal. The cost is
  accepted knowingly — a console reading only the subresource sees absence without a reason, which is
  why G6 states where the reason *is* discoverable rather than claiming it always is.
- **Record the partition's identifier in the allocation** so no reverse lookup is needed. Rejected
  here, then **adopted** once the shipped code was measured: the estimate this rejection rested on —
  `1 + 2N` calls per MIG card per period, `N ≤ 7` — left out the profile catalog probe, which asks
  the driver about every one of the 17 GPU-instance profile ids, twice each, only to translate a
  recorded profile *name* back into a driver profile *id*. The real cost is ~76 NVML calls per MIG
  card per 15 s period, and T-Head's library enumerates 85 profile ids rather than 17, so a partition
  reader for it could not be written this way at all. The allocator already reads the identifier when
  it creates or adopts the partition, and already persists it in its own marker file. The reverse
  lookup is not kept as a fallback: an allocation recorded before the field is answered as an absence
  with a reason, which is the honest reading — the parent card's figures belong to every tenant on it.

  What this does **not** buy, contrary to the argument that first suggested it: these identifiers are
  name-based, derived from the parent device and the instance's own identity, so destroying a
  partition and creating another at the same placement returns the *same* identifier — observed
  directly on an H100. Addressing by identifier is therefore exactly as strict as matching by
  placement, and neither distinguishes one generation of a partition from the next.
- **Gate on Pod readiness.** Rejected: a `Running`-but-unready Pod can be holding accelerator memory, so
  reporting zero would fabricate an idle measurement. The predicate is "has any container started".
- **Clamp `memoryUsedMiB` to `memoryTotalMiB`.** Rejected: it would present every leaking quota as a
  perfectly enforced one.
- **Publish a partition block with `coresTotalPercent: 0` when the cap cannot be read.** Rejected: the
  field is a value rather than a pointer, so a block must carry a number, and zero would be a
  fabricated quota where F2 promises a populated one. No block is published instead. The cost is named
  and deferred rather than hidden: a T-Head PPU partition's `memoryTotalMiB` is derivable but stays
  unpublished for want of a compute cap. That costs nothing today, since no T-Head partition reader
  exists to publish it from; when one is written, the honest fix is to reconsider whether the field
  should be optional, not to invent a cap for it.
- **Keep `503` for a non-running Instance.** Rejected: it leaves every consumer writing the same branch,
  and an Instance with no started container genuinely consumes nothing.

## Open Questions

- ~~Is a per-vNPU query reachable on Ascend (F6 layer 2)?~~ **Answered: no.** The struct exists but
  only as an embedded field, no function in either our header or the driver 25.5.1 header takes the
  struct that wraps it, and `libdcmi.so` exports no such query. Ascend serves per-process memory and
  no compute.
- **Per-process compute utilization beyond NVIDIA, Cambricon and T-Head.** The other bindings expose
  memory but not utilization; is there a vendor call the bindings do not generate?
- **Attribution across runtimes.** T1's table covers the cgroup shapes we know; the set of runtimes we
  actually ship onto — and whether any of them hides the vendor's PIDs entirely — is itself open until
  T1's spike reports.
- **Should the snapshot's tenant section eventually move behind authentication?** F7 documents the
  exposure and recommends the NetworkPolicy; adding authentication to the Device Manager's secure port
  is a larger change with its own spec.
