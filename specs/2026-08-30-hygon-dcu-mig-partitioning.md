# Spec: Hygon DCU hardware partitioning (MIG)

Status: Specified
Type: Feature

## Summary

Teach GPUStack to schedule Hygon DCU **hardware partitions**. Hygon ships a Multi-Instance
management API in `libhydmi_mig.so`, carving a card into GPU Instances (GI) and Compute Instances
(CI) the way NVIDIA MIG and T-Head do. This spec adds the Go binding for that library, the detector
that publishes each card's partition profiles, the allocator that reserves an instance and injects it
into a workload container, per-instance metrics, operator documentation, and end-to-end cases.

Hygon's mode switch is **system-wide**, not per-card, and a MIG-enabled node cannot serve whole-card
or logically-sliced work at all. That single fact — measured, not inferred — makes Hygon partitioning
a *node-level exclusive mode* rather than the per-card, mixed-population model NVIDIA and T-Head
follow, and it is the reason several of the existing partition abstractions cannot be reused as-is.

## Motivation

### Goals

- **G1 — Binding.** A Go binding over `libhydmi_mig.so.1` that cannot collide with `binding/nvml`,
  covering system/device MIG mode, GI and CI profiles, possible placements, remaining capacity,
  instance enumeration, create/destroy (with and without placement), and MIG-device handles.
- **G2 — Detection.** The device manager reports, per Hygon card: whether the node is in MIG mode,
  the card's partition profile inventory (name, memory, compute slices, memory slices, max count,
  legal placements), and the instances that currently exist.
- **G3 — Allocation.** A partition request is served by reserving (or creating) a GI+CI of the
  requested profile on a card that can host it, and injecting it into the container. Releasing a
  workload reclaims the instance.
- **G4 — Metrics.** Per-instance memory and compute utilisation reach the `Devices` subresource, the
  exporter and `/monitor/snapshot`, the way the whole-card and logical-slice figures already do.
- **G5 — Documentation.** A `docs/reference/hygon-mig.md` operations page alongside `nvidia-mig.md`
  and `thead-mig.md`, covering the node-level mode, the forced teardown order, and the failure modes
  an operator will actually hit.
- **G6 — End-to-end coverage.** Cases for the allocation shapes Hygon can serve, plus explicit
  negative cases for the shapes it cannot.

Success is measured on hardware: an 8-card BW (C-3000, gfx936) node in MIG mode admits partition
requests, runs a real HIP workload inside the partition, reports metrics matching the profile
geometry, and reclaims cleanly.

### Non-Goals

- **Toggling MIG mode from the operator.** The switch is system-wide and refuses to run while any
  driver client holds the device — including the device manager's own process. Flipping it is a node
  provisioning action, like installing the driver. The operator *detects and adapts*; it never flips.
- **Serving one workload from several MIG instances.** The vendor stack caps a container at one
  visible instance (see the evidence below). This is a vendor limitation, not a backlog item.
- **Mixed populations on one node.** Not expressible: the mode is system-wide.
- **Coexistence of MIG with logical slicing on the same node.** A MIG-enabled node serves partitions
  only.
- Changing the logical-slicing path, which already works and stays the non-MIG answer.

## Proposal

A Hygon node is in exactly one of two states, and the device manager publishes accordingly:

| Node MIG mode | Card is usable as | Node advertises |
| --- | --- | --- |
| Disabled (default) | whole card, or logical slices | whole-card + logical-slice keys (today's behaviour, unchanged) |
| Enabled | MIG instances only | partition-profile keys only |

In MIG mode the allocator owns the **per-card** instance lifecycle: it creates a GI+CI to satisfy a
request, injects it, and destroys it on release. That is safe while other cards run workloads — only
the system-wide mode switch demands a quiet node.

### User Stories

#### Story 1
As a platform operator with Hygon DCU nodes, I want to put a node into MIG mode and have GPUStack
publish its partition profiles automatically, so that I do not have to describe the hardware by hand.

#### Story 2
As a user, I want to request a `2g.15gb` Hygon partition and get a container that sees exactly one
device with 20 CU and 16380 MiB, so that my job gets the isolation the profile promises.

#### Story 3
As a user, I want several partitions carved from one card to run as independent workloads
concurrently without interfering, so that a 63 GB card serves four small jobs.

#### Story 4
As an operator, I want a request GPUStack cannot serve — a workload asking for more than one Hygon
MIG instance — to be refused clearly at admission, so that I learn it from an error message rather
than from a Pod that starts and finds no device.

#### Story 5
As an operator, I want per-instance memory and compute figures for a running partition, so that I can
tell a busy partition from an idle one without shelling into the node.

#### Story 6
As an operator, I want a page that tells me the teardown order and why enabling MIG on a node with no
instances makes every card unusable, so that I do not brick a node during a maintenance window.

### Core Features & Acceptance Criteria

**F1 — `binding/dmi`, a collision-proof binding.**
- Generated into its own package; it never emits a reference to a `nvml*` C symbol.
- Loads `libhydmi_mig.so.1` with `RTLD_LOCAL`, so the vendor's `nvml*` names never enter the global
  symbol namespace.
- Acceptance: a binary linking both `binding/nvml` and `binding/dmi` builds, and a `dmi` call
  resolves to the Hygon library even when an NVML library is loaded in the same process.
- Acceptance: the profile-index quirk is encoded — the `profile` argument is the **slice count minus
  one**, and an unsupported index is a routine gap, not an error that aborts the sweep.

**F2 — Detection.**
- The node's MIG mode is read once per detect pass and recorded as a card capability.
- With MIG enabled, each card publishes its profile inventory built from
  `GetGpuInstanceProfileInfo` + `GetGpuInstancePossiblePlacements`, mapped onto
  `device.AcceleratorPhysicalSlicedProfile`.
- Acceptance on hardware (BW card): three profiles are published — `2g.15gb` (1 slice, 20 CU,
  16380 MiB, max 4, placements `0:1 1:1 2:1 3:1`), `4g.31gb` (2 slices, 40 CU, 32760 MiB, max 2,
  placements `0:2 2:2`), `8g.63gb` (4 slices, 80 CU, 65520 MiB, max 1, placement `0:4`).
- Acceptance: with MIG enabled the card publishes **no** whole-card or logical-slice capability, and
  with MIG disabled it publishes exactly what it publishes today.

**F3 — Allocation.**
- A partition request reserves a GI+CI of the requested profile, choosing a placement that does not
  overlap a live instance.
- The container receives: the CI conf bind-mounted at its own host path read-only, the three device
  nodes `/dev/dri`, `/dev/kfd`, `/dev/mkfd`, a MIG-capable `/opt/hyhal`, and
  `DMI_MIG_VISIBLE_DEVICE=MIG-<uuid>`.
- Acceptance: four `2g.15gb` partitions carved from one card serve four concurrent containers, each
  seeing exactly one 20 CU / 16380 MiB device.
- Acceptance: a request for more than one Hygon MIG instance is refused at admission with a message
  naming the limitation.
- Acceptance: release destroys CI then GI; a partition still in use is never destroyed.

**F4 — Metrics.** Per-instance total/used memory and compute utilisation reach the `Devices`
subresource, the exporter and `/monitor/snapshot`. A figure the library refuses is reported with a
reason rather than as a zero.

**F5 — `docs/reference/hygon-mig.md`.** Covers the system-wide mode and why it is out of the
operator's hands, the exclusivity with whole-card and logical slicing, the profile table, the forced
CI→GI→mode teardown order, and the failure modes below.

**F6 — End-to-end cases.** Positive: one partition; several partitions on one card; metrics on a held
partition; reclaim. Negative: a multi-instance request refused; a whole-card request refused on a
MIG-enabled node. Every case restores the node to the state it found.

### Notes / Constraints / Caveats

All of the following were measured on an 8-card BW (C-3000, gfx936, 4 slices/card, DTK 25.04.4)
node on 2026-08-30. They are constraints on the design, not background reading.

**C1 — The mode switch is system-wide.** `hy-smi -mig <0|1>` (long form `--multi-instance-gpu`; the
manual's `--multi-instance-dcu` screenshot is wrong) rejects `-i` with `Invlaid arguments`. It maps
to `nvmlSetSystemMigMode`. There is no per-card mode.

**C2 — Any driver client blocks the switch.** With the device manager holding `/dev/kfd`, enabling
fails with `Set system level mig mode failed. The device is exist and may be in use.` The DaemonSet
has to be parked first. `/dev/mkfd` stays held by the vendor's own `hymgr` daemon, which is fine.

**C3 — A MIG-enabled node cannot serve whole cards.** A container given the device nodes and a
MIG-capable `/opt/hyhal` but **no** CI conf reports `No HIP GPUs are available`, even with five of
eight cards holding no instances at all. MIG mode is exclusive at the node level.

**C4 — A container sees exactly one MIG instance.** Mounting two CI confs, passing a comma-separated
list of `MIG-`-prefixed UUIDs, or passing `all`, all yield `device_count == 1`. Reproduced on DTK
25.04 and DTK 26.04, so it is not a version artefact.

**C5 — The per-card instance lifecycle is concurrency-safe.** With a workload actively holding
card 0's instance, creating and destroying a GI+CI on card 3 both succeed, while destroying the busy
instance is correctly refused with `The device is exist and may be in use.`

**C6 — The library's symbols are NVML's.** Every exported symbol is `nvml*` and every type is
`nvmlDevice_t` / `nvmlReturn_t` / `nvmlGpuInstance_t`; the return enum is NVML's verbatim. The
**structs are not** — `nvmlGpuInstanceProfileInfo_t` is
`{id, gi_count_max, cu_count, gpu_slice_count, memory_size_MB, name[256]}`, a different layout and
field set from NVIDIA's. The generated `binding/*` code calls C symbols *by name* and
`binding/types.go` dlopens with `RTLD_LAZY|RTLD_GLOBAL`, so a second package emitting the same names
would resolve against whichever library reached the global scope first.

**C7 — Three identity gaps.** `nvmlDeviceGetUUID` **does not exist** as a symbol; `GetName` returns
`INVALID_ARGUMENT` with an empty buffer; `GetPciInfo` returns success and writes an empty string.
So the instance UUID comes from the CI conf's trailing `mig_uuid:` line, the card name still comes
from HSA, and the device-index→BDF map is the one-line file `/etc/dmi_mig_config/dev<N>`.

**C8 — Two enumeration rules.** `GetGpuInstanceProfileInfo`'s `profile` argument is the slice count
minus one (`0`→id 3/1 slice, `1`→id 1/2 slices, `2`→unsupported, `3`→id 0/4 slices), so gaps are
normal. `GetGpuInstances` filters by profile id and must be called once per id.
`GetGpuInstanceRemainingCapacity` is accurate and can be trusted.

**C9 — GI and CI ids are driver-assigned** and bear no relation to placement: four `2g.15gb` GIs came
back as ids 3, 4, 5, 6 sitting at placements 0, 1, 2, 3.

**C10 — The container needs the host's `/opt/hyhal`.** A stock DTK 25.04 image's bundled copy has no
`libhydmi.so`; the runtime then prints `open hydmilib:libhydmi.so error` and fails with
`No HIP GPUs are available`.

**C11 — The CI conf is the unit of injection.** It lives at
`/etc/dmi_mig_config/ci/dev<N>gi<G>ci<C>.conf`, is the concatenation of the GI block and the CI
block, and ends with `mig_uuid:`. It must be mounted at the identical path, read-only; the vendor
warns that modifying it prevents the instance from activating.

### Boundaries

- **Always:**
  - Treat the node's MIG mode as read-only input to detection.
  - Destroy in the order CI → GI; never destroy an instance a workload holds.
  - Leave a node exactly as found after any e2e run — same mode, same instances.
  - Load the vendor library with `RTLD_LOCAL`.
  - Sign every commit off.
- **Ask first:**
  - Any change to the logical-slicing path, which is shipped and working.
  - Any change to the shared `_partition-lib.sh` that the NVIDIA and T-Head cases depend on.
- **Never:**
  - Toggle MIG mode from operator code.
  - Leave a node in MIG mode with zero instances — every card is then unusable.
  - Reuse `binding/nvml`'s Go types for this library.
  - Write a host address into the spec, docs, commit messages or any e2e artefact.

### Risks and Mitigations

- **C-symbol collision with NVML silently misroutes calls** → the `binding/dcmi` wrapper pattern
  (`.def` macro list, `w_`-prefixed wrappers, per-handle `dlsym`) plus `RTLD_LOCAL`; a test asserts
  the binding never emits a bare `nvml*` reference.
- **Struct layout drift between the vendor header and the Go types corrupts memory** → the layout is
  generated from the vendored header, never hand-written; `cgocheck` discipline applies (never pass
  `&struct.field` into cgo).
- **A node left in MIG mode with no instances is bricked for every user** → e2e cases restore state
  in a trap; the docs page leads with this failure mode.
- **The exclusivity (C3) surprises an operator who expects NVIDIA's mixed model** → detection stops
  publishing whole-card capability the moment MIG mode is on, so the node's advertised capacity tells
  the truth immediately; the docs page states the difference explicitly.
- **The one-instance cap (C4) makes an otherwise reasonable request unservable** → refuse it at
  admission with a message that names the limitation, and cover the refusal with an e2e case.
- **The shared partition e2e lib is NVIDIA-shaped** (`nvidia-smi` in `mig_mode`, `set_mig_mode`,
  `node_gi_count`) → introduce the Hygon operations behind the same function contracts rather than
  editing the NVIDIA paths, so the existing cases keep their behaviour.

## Design Details

### Commands

```bash
# Build / generate
make generate                # regenerates every binding; carry back only binding/dmi
go build ./...
go vet ./pkg/... ./binding/dmi/...

# Lint (edit pass — rewrites the whole module; cold run exceeds 2 minutes)
make lint
make lint docs

# Unit tests
go test ./pkg/devicemanager/detector/hygon/... ./pkg/devicemanager/allocator/hygon/... ./binding/dmi/...

# Hardware verification (run against the Hygon lab node; address supplied at run time)
XB_MODE=ssh XB_HOST=<node> ./.claude/skills/gpustack-operator-xbuild-and-verify/cases/hygon-case-N.sh
MIG_NODE_SSH=<node> ./.claude/skills/gpustack-operator-e2e/cases/run-partition-block.sh <RAW_DIR>
```

### Project Structure

```
gen/binding/dmi/
  config.yaml               # c-for-go config, PackageName dmi, includes the wrapper header
  dmi_mig.h                 # vendored vendor header (1466 lines)
  dmi_mig_wrapper.{h,c}     # hand-written dlsym dispatch, mirrors binding/dcmi
  dmi_mig_wrapper_api.def   # X(ret, name, decl_args, call_args) macro list
binding/dmi/                # generated output + hand-written library.go
pkg/devicemanager/detector/hygon/
  mig_profile.go            # profile inventory, placements, mode detection
  mig_process.go            # per-instance metrics
pkg/devicemanager/allocator/hygon/
  mig.go                    # reserve / adopt / release, placement selection
  mig_visibility.go         # CI conf mount, device nodes, DMI_MIG_VISIBLE_DEVICE
docs/reference/hygon-mig.md
.claude/skills/gpustack-operator-e2e/cases/   # new Hygon partition cases
```

### Code Style

The wrapper follows `binding/dcmi` exactly — the macro list is the single source of truth, so no
per-function boilerplate is hand-maintained and nothing references the vendor's `nvml*` symbols at
link time:

```c
// dmi_mig_wrapper_api.def
#define DMI_MIG_API_LIST(X) \
    X(int, nvmlGetSystemMigMode, (unsigned int *current, unsigned int *pending), (current, pending)) \
    X(int, nvmlDeviceGetGpuInstanceProfileInfo, \
      (nvmlDevice_t device, unsigned int profile, nvmlGpuInstanceProfileInfo_t *info), \
      (device, profile, info)) \
    /* ... */

// dmi_mig_wrapper.c
#define W_FUNC(ret, name, decl_args, call_args)      \
    ret w_##name decl_args {                         \
        if (!name##_func) return ERROR_FUNCTION_NOT_FOUND; \
        return name##_func call_args;                \
    }
DMI_MIG_API_LIST(W_FUNC)
```

Go conventions follow the surrounding detector code: snake_case multi-word filenames, injected query
functions so derivations stay hardware-free and unit-testable, and a returned reason (rather than a
logged one) wherever a profile is refused — the T-Head `deriveSlicedProfiles` shape.

### Implementation Plan

Tasks are ordered by dependency. T1 blocks everything; T2 blocks T3 and T4; T5 and T6 close out.

**T1 — `binding/dmi` over `libhydmi_mig.so.1`.**
`Owns:` `gen/binding/dmi/**`, `binding/dmi/**`
- Vendor `dmi_mig.h` into `gen/binding/dmi/`.
- Write `dmi_mig_wrapper_api.def` (the `X(ret, name, decl_args, call_args)` list), `dmi_mig_wrapper.h`
  (the `w_` declarations) and `dmi_mig_wrapper.c` (static function pointers, `RTLD_LOCAL` dlopen,
  `dlsym` resolution, thread-local last-error), mirroring `binding/dcmi`.
- Add `gen/binding/dmi/config.yaml` — `PackageName: dmi`, includes the wrapper header, accepts
  `^w_nvml` functions and strips the `w_` prefix.
- Hand-write `binding/dmi/library.go`: `New` with the `/opt/hyhal/lib` candidate list, `Init`,
  `Shutdown`, and Go-level helpers for the two quirks — profile index = slice count − 1, and
  per-profile-id instance enumeration.
- Verify: the whole tree builds; `nm` on a test binary shows no undefined `nvml*` symbol coming from
  this package.

**T2 — Detector: node MIG mode and the profile inventory.**
`Owns:` `pkg/devicemanager/detector/hygon/mig_profile.go`, `device.go`
- Read the system MIG mode once per detect pass.
- Probe profiles across the slice-count index space, tolerating the unsupported index as a gap;
  resolve each profile's possible placements; derive `AcceleratorPhysicalSlicedProfile`, reusing the
  T-Head refusal discipline (returned reasons, not logged ones).
- Map the device index to its BDF through `/etc/dmi_mig_config/dev<N>` for identity.
- With MIG on, publish `PhysicalSliced` and suppress the whole-card and logical-slice capability;
  with MIG off, leave today's behaviour untouched.

**T3 — Allocator: reserve, inject, release.**
`Owns:` `pkg/devicemanager/allocator/hygon/mig.go`, `mig_visibility.go`
- Select a free placement for the requested profile, create GI then CI, and record ownership so a
  restarted device manager re-adopts rather than double-allocates.
- Injection: bind-mount the CI conf at its own host path read-only, add `/dev/dri`, `/dev/kfd`,
  `/dev/mkfd`, ensure a MIG-capable `/opt/hyhal`, set `DMI_MIG_VISIBLE_DEVICE=MIG-<uuid>` (the UUID
  read from the conf's `mig_uuid:` line).
- Release destroys CI then GI, and never touches an instance still in use.
- Refuse a request for more than one Hygon MIG instance at admission.

**T4 — Metrics.**
`Owns:` `pkg/devicemanager/detector/hygon/mig_process.go`
- Per-instance memory and compute through the MIG device handles, carried up through the `Devices`
  subresource, the exporter and `/monitor/snapshot`; a refused figure carries a reason.

**T5 — Documentation.**
`Owns:` `docs/reference/hygon-mig.md`, `docs/README.md`, `docs/architecture/device-discovery.md`
- The operations page, the index entry, and the cross-links.

**T6 — End-to-end cases.**
`Owns:` `.claude/skills/gpustack-operator-e2e/cases/**`
- Introduce the Hygon hardware operations behind the existing `_partition-lib.sh` function contracts
  without changing the NVIDIA paths, and add the cases listed in the Test Plan.

### Test Plan

**Unit.** Table-driven, following the existing detector/allocator tests:
- Profile derivation: the index→profile-id mapping including the unsupported gap; a profile with no
  legal placement is refused; two names normalising to one with differing geometry are withheld.
- Placement selection: a placement overlapping a live instance is never chosen; a full card yields no
  placement.
- Conf parsing: `mig_uuid` extraction; a truncated or modified conf is rejected rather than
  half-read.
- Injection shape: the mount path equals the host path, is read-only, and the env value carries the
  `MIG-` prefix.
- Admission: a request for two Hygon MIG instances is refused with a message naming the limitation.

**Hardware (the verdict — nothing here is settled by a unit test).** On the 8-card BW node:
1. MIG off → the node advertises whole-card and logical-slice keys exactly as it does today.
2. MIG on → the node advertises the three profile keys and **no** whole-card key.
3. One `2g.15gb` partition runs a real HIP workload reporting 20 CU / 16380 MiB.
4. Four `2g.15gb` partitions from one card serve four concurrent containers.
5. A `4g.31gb` and an `8g.63gb` partition report 40 CU / 32760 MiB and 80 CU / 65520 MiB.
6. Metrics on a held partition are non-zero and agree with the profile geometry.
7. Release reclaims CI then GI; the card returns to its previous instance count.
8. A two-instance request is refused at admission.
9. A whole-card request on a MIG-enabled node is refused.
10. Every case restores the node's mode and instances in a trap.

## Alternatives

- **Generate `binding/dmi` straight from the vendor header, as `binding/nvml` is generated.**
  Rejected: the generated code would call `C.nvmlDeviceGetCount` by name, and with
  `RTLD_LAZY|RTLD_GLOBAL` a process holding both libraries resolves one package's calls into the
  other's library. The wrapper is the only shape that makes the collision impossible rather than
  unlikely.
- **Rename the symbols in the vendored header.** Rejected: the renamed symbol would not exist in the
  shared object, so every call would fail to resolve at runtime.
- **Drive MIG through `hy-smi` instead of the library.** Rejected: parsing an ASCII-art table is
  brittle, the tool cannot run inside the device-manager container, and the library answers every
  query the detector needs directly.
- **Model Hygon MIG per-card like NVIDIA's, treating a card with no instances as "not partitioned".**
  Rejected: C3 shows such a card is unusable, so the model would advertise capacity that cannot be
  served.
- **Let the operator toggle MIG mode when a partition request arrives.** Rejected: the switch is
  system-wide, needs every driver client gone (including the device manager itself), and would make
  every other workload on the node fail. It is a provisioning action.
- **Serve one workload from several MIG instances by mounting several CI confs.** Rejected: measured
  not to work (C4).

## Open Questions

1. **Does logical slicing survive MIG mode?** C3 shows whole-card access does not. The
   `vdev<N>.conf` path goes through the same HSA layer and is expected to fail too, but this has not
   been measured. It only affects how the exclusivity is *described*, not whether it exists — the
   node will publish partition keys only either way. To be settled early in the build.
2. **How should a node's MIG mode reach the scheduling chain — a card capability, a node label, or
   both?** The existing partition families are per-card, so a node-level fact has no established
   home. Leaning toward recording it as a card capability (every card on the node agrees by
   construction) so nothing downstream needs a new concept.
3. **Should a partition-capable Hygon node still advertise its logical-slice keys when MIG is off?**
   Yes by default — that is today's behaviour and this spec does not change it — but it means one
   node model produces two very different advertisements depending on a mode the operator sets out of
   band. Worth a docs callout.
4. **Is `/opt/hyhal` already injected into workload containers on a MIG node by the operator's
   existing Hygon path, or must the allocator add it?** C10 makes it required; where it comes from is
   an implementation detail to confirm against the current injection code.
