# Spec: NVIDIA scale-up fabric domain

Status: Shipped
Type: Feature

## Summary

`Devices` already records, per accelerator, the scale-up interconnect domain it belongs to —
`topology.fabric`, carrying a kind, a domain id that is comparable across machines, and where the
domain is partitioned, a clique. Until now exactly one detector filled it. On every NVIDIA node the
field was absent and `feature.gpustack.ai/fabric.domain` was never published, which left out the one
manufacturer whose driver answers a **node-local read with a cross-node identity**.

This makes the NVIDIA detector read that identity. NVML reports, per GPU, the cluster the fabric
manager has registered it with and the clique inside that cluster; the detector publishes them as
`kind: nvlink`, `id: <cluster uuid>` and `cliqueId`, and the existing label construction turns a
node-wide agreement into the node label.

The feature is mostly its gate. NVML reports a **registration state** beside the two identifiers, and
only one of its four values means they have been filled in at all. Publishing the other three would
hand one domain to every machine whose driver left those bytes alone — and this id exists to be
compared across machines, which is the only reason publishing it is worth anything.

No CRD, no new API field, no new label key: each already exists, and each was designed for this
manufacturer among others.

## Motivation

### Dependencies

Each was verified present on `main` at `5e2b617d`, this branch's base.

| # | Prerequisite | Where it lives | Verified |
|---|---|---|---|
| D1 | `DeviceFabric` — the API face, with `kind` in {`ub`,`nvlink`,`xgmi`}, `id`, `cliqueId` | `api/worker/v1alpha1/devices.go:187` | read |
| D2 | `ConstructFabricNodeLabels` and its node-wide reduction | `pkg/nodefeature/fabric.go:48` | read |
| D3 | That reduction is already wired into the detector's label pass | `pkg/devicemanager/detector/detector.go:727` | read |
| D4 | The NVML fabric entry point, both struct versions | `binding/nvml/library_device.go:319` | read |
| D5 | `GpuFabricInfo{ClusterUuid [16]uint8, Status, CliqueId, State}` | `binding/nvml/zz_generated.types.go:988` | read |
| D6 | The four registration states as named constants | `binding/nvml/const.go:930-937` | read |
| D7 | The vendor's own rules in the vendored header: `status` is "to be checked only if state returns complete"; v1 is "Deprecated: will be deprecated in a future release" | `binding/nvml/nvml.h:3462`, `:7194` | read |
| D8 | A complete precedent for the same field on another manufacturer | `pkg/devicemanager/detector/ascend/fabric.go:36` | read |
| D9 | The site where the NVIDIA detector builds a topology per accelerator | `pkg/devicemanager/detector/nvidia/device.go:202` | read |

There is no unmerged prerequisite, and that is why this work was selectable at all.

### Goals

- An NVIDIA accelerator that has registered with a fabric manager records
  `spec.groups[].accelerators[].topology.fabric` with `kind: nvlink`, the cluster uuid as `id`, and
  its clique as `cliqueId`.
- Such a node publishes `feature.gpustack.ai/fabric.domain`, so a `ResourceFlavor` can pin "same
  NVLink domain" exactly as it can already pin "same Ascend super pod".
- An accelerator that has **not** finished registering records nothing, and its node publishes
  nothing. This is the measurable success criterion: three of the four states publish no record.
- The label's value distinguishes cliques, so it never promises co-location across a partition.
- Ascend's published behaviour is byte-identical to what it was.

### Non-Goals

- **The cross-node join.** This publishes the per-node half in a shape a join can consume. Assembling
  the cluster-wide picture is a controller's or scheduler's job and is not attempted here.
- **An external topology service.** Its accelerator-domain discovery covers one manufacturer, and
  under Kubernetes it execs into another DaemonSet's pod to run a vendor CLI. Reading NVML directly
  costs no exec and no external dependency.
- **A pairwise, node-local NVLink topology.** `GetTopologyCommonAncestor` answers a different
  question — which two GPUs on *this* machine are near each other — and the axis that would have held
  it was deliberately withdrawn once already.
- **New label keys.** `fabric.domain` and `fabric.members` are defined and documented; nothing here
  adds a third.
- **AMD XGMI, or any further manufacturer.** Same path, separate work.
- **Hardware verification as a completion gate.** See Risks and Mitigations.

## Proposal

### What is already there, measured

`DeviceFabric` was introduced for several manufacturers and its field comments already say which one
fills each field: `id` is the super pod id on Ascend, the fabric cluster uuid on NVIDIA and the XGMI
hive id on AMD, and `cliqueId` is documented as **NVIDIA-only and load-bearing** — "two GPUs sharing
ID but not CliqueID are in one fabric and still cannot reach each other". The producer for the
NVIDIA half was the only piece missing.

The label side is likewise complete. `ConstructFabricNodeLabels` reduces the node's whole inventory
to one domain or to none, withholds on any disagreement, and is already called from the detector's
label pass. It reads `Fabric` and knows nothing about manufacturers.

### The registration state is the feature

NVML's answer carries four states: `NOT_SUPPORTED`, `NOT_STARTED`, `IN_PROGRESS`, `COMPLETED`. Only
the last means the fabric manager has filled in the cluster uuid and the clique; the first says this
generation has no such fabric, and the middle two say registration is pending or under way.

The three non-final states are not "slightly stale" — nothing has assigned the accelerator a cluster
yet. An ordinary card reports them as zeros, measured; what a card mid-registration reports is not
established here. Either way a detector that published them would give **every machine in that state
one shared domain**, and a flavor pinning it would co-locate a job across machines with no
interconnect between them. That is the exact failure the `id` field's own comment forbids by
requiring the value be comparable across workers.

The vendor attaches a second half to the same gate: `status` "must be checked only if state returns
complete". A registration that completed unsuccessfully identifies no cluster, so both halves are
checked, in that order.

A third case survives both: an **all-zero cluster uuid**. Sixteen zero bytes are the shape of a
buffer nobody filled rather than the identity of a cluster, and every machine reporting it would
again land in one shared domain. It is refused for the same reason and by the same rule.

### The gate agrees with the manufacturer's own reference implementation

The three checks above were derived from the vendored header and from what the `id` field promises,
and were then compared against the manufacturer's own library. `IsFabricAttached` in
`NVIDIA/go-nvlib`, `pkg/nvlib/device/device.go`, requires exactly the same three and in the same
sense: the state is `COMPLETED`, the cluster uuid is not the zero array, and the status is success.
It also treats a not-supported return as a plain "no" rather than as an error, which is how the
refused read is handled here.

This is recorded because the third check is the one nothing documents. The header says nothing about
a zero uuid; it was added from the `id` field's cross-worker requirement, and finding the same check
in the manufacturer's library is independent corroboration rather than the source of it.

### The clique belongs in the label's value

`fabric.domain` is published as `<kind>-<id>`. For a manufacturer that partitions a domain, that
value promises more than the hardware delivers: two nodes holding the same cluster uuid on opposite
sides of a partition publish the same value while being unable to address each other, and an
equality selector would put one job across both.

The partition is not a theoretical shape. The clique id **is** the NVLink partition id, and
partitioning a rack-scale system into several isolated domains is an administrator-facing operation;
the vendor's own documentation states that compute trays in one partition cannot establish
multi-node NVLink connections with trays in another.

The value therefore becomes `<kind>-<id>-<clique>` wherever a clique is reported, and stays
`<kind>-<id>` wherever none is — which is every manufacturer but NVIDIA today, so no published value
changes. **The window for this is now and only now**: no NVIDIA node has ever published this key, so
composing the value costs nothing today and would be a breaking change to every written selector
after the first release that publishes one.

Three of the manufacturer's own components reach the same shape from the other side. Its Kubernetes
device plugin joins the cluster uuid and the clique id into one node label value and withholds that
label when a node's accelerators disagree on either. Its topology service parses the same two fields
out of a host CLI, joins them the same way, refuses the marker an unregistered accelerator reports,
and calls a node carrying more than one of them ambiguous rather than picking one. And its cloud
integration states the reason in a comment: group on the whole value, because that is **the
granularity a job must stay inside to get NVLink bandwidth**, and it is finer than the cluster uuid
alone since one rack can be split into several cliques. The keys and separators differ; the identity
being published is the same pair, and the rule for withholding it is the same rule.

**One difference from those implementations is deliberate.** The device plugin skips an accelerator
that is not fabric-attached and publishes the clique of the ones that are; this reduction treats a
missing record as disagreement and publishes nothing. The two answer different questions: theirs
names the fabric some of the node's accelerators are on, ours promises that **every** accelerator on
the node is in the named domain, which is what makes it safe as an equality selector for placing a
whole workload. That promise predates this change and is what the Ascend path already relies on.

The clique is folded into the value rather than compared beside it, so that the node-level
disagreement rule follows without a second mechanism: a node holding two cliques of one cluster now
disagrees with itself and publishes no key, exactly as a node holding two different clusters does.

### A refused read is not a withdrawal

Withholding a label **removes** it. A pass whose NVML read failed therefore publishes the last
registration that accelerator answered with, keyed by its UUID, rather than taking the node out of a
domain it has not left until the next detect pass — which the detect floor can hold off for minutes.
This is the field's own documented contract, "only an answer the driver gave replaces it", and the
rule the Ascend detector already follows.

An accelerator that really leaves the fabric is a *different answer*, not an absent one: the driver
reports a state that is no longer complete, the remembered value is replaced, and the withdrawal
happens through a read that succeeded.

### User Stories

#### Story 1
As a cluster operator running multi-node NVLink hardware, I want the nodes of one fabric domain to
carry one node label, so that a `ResourceFlavor` can pin a distributed workload to machines that can
actually address each other's memory.

#### Story 2
As an operator whose node is *not* selected, I want the detector to report which registration state
it read, so that "the fabric manager has not finished registering this GPU" is distinguishable from
"this card has no fabric" — knowing that the published object itself cannot tell me, because it
records both as no record at all.

#### Story 3
As a maintainer adding the next manufacturer's fabric, I want one paradigm rather than two, so that
the XGMI hive id is a reader plus a gate and not a fresh design.

### Core Features & Acceptance Criteria

**F1 — The detector reads the fabric registration per accelerator.**
Newest struct version first, older as fallback: the vendor marks v1 for removal, and a driver serving
only one of the two still answers. The newer path did not work before this change — the binding
probed for a symbol the vendor never exports — so repairing that probe belongs to F1 rather than to a
separate cleanup. Acceptance: a registered accelerator records `topology.fabric.kind == "nvlink"`
with a non-empty `id` and `cliqueId`.

**F2 — Three of the four states publish nothing, and so does a failed status.**
Acceptance: unit cases for all four states, three asserting no record; a case for
`state == COMPLETED` with a non-success status asserting no record. Every negative case carries a
cluster uuid and clique that *would* be published, so it differs from the positive baseline in
exactly the field it is about.

**F3 — The domain id encoding is pinned.**
All 16 bytes, in order, lowercase hex, undelimited — 32 characters. Acceptance: an assertion on the
width, the case and the absence of delimiters, over bytes that are neither palindromic nor uniform.
Rationale rather than preference: two workers rendering the same bytes differently are two domains.

**F4 — An all-zero cluster uuid publishes nothing.** Acceptance: a case at `COMPLETED` with a success
status and a zero uuid asserting no record.

**F5 — A refused read reuses the last answer; a successful one always replaces it.**
Acceptance: a unit test walking nothing-remembered, remembered, refused, per-accelerator isolation,
and withdrawal through a successful read that is no longer complete.

**F6 — The node label distinguishes cliques.**
Acceptance: `nvlink-<id>-<clique>` for agreeing accelerators; no key at all when two cliques of one
cluster sit on one node; `ub-7` unchanged for a manufacturer reporting no clique; clique `0` rendered
like any other clique rather than dropping to the two-part form.

**F7 — The record's documentation stops saying one manufacturer fills it.**
`DeviceFabric`'s comment, the generated copies of it, and the manufacturer table in
`docs/architecture/network-topology.md` all state the new truth. Acceptance: no surviving sentence in
the repository claims only Ascend fills this field.

### Notes / Constraints / Caveats

- **Both struct versions are asked, newest first.** The cost falls on cards with no fabric: two
  refusals per pass rather than one. Neither is a failure, and both are reported at the detector's own
  verbosity, because a card whose generation has no fabric answers this way on every pass forever.
  The manufacturer's own library asks in the opposite order; nothing depends on which.
- **The newer entry point was unreachable until this change.** The binding's handler probed for
  `nvmlDeviceGetGpuFabricInfo_v2`, following the `_v2` suffix its siblings use, and the vendor
  declares no function of that name: for this call the versioned entry point is spelled with a
  trailing `V`, and the `_v2` suffix belongs to the struct version macro instead. The probe therefore
  failed on every driver and the method returned "function not found" unconditionally. The three
  sibling probes in that file name symbols the header really declares, which is what makes this one a
  typo rather than a convention.
- **Which of the two paths answered is not observable from the record**, and that is why the broken
  probe survived: both paths produce the same record, so a caller with a fallback cannot tell "the
  newer call is missing" from "the probe names nothing". Nothing reports the successful path — that
  would be instrumenting product code to watch a branch. A **refusal** of the newer call is reported,
  at `V(3)`, because a struct that stops matching what the driver writes would otherwise reach a
  consumer as silence. The repair itself is confirmed against a driver's exported symbols rather than
  by anything the detector emits.
- **A card with no fabric answers the query rather than refusing it**, which is the opposite of what
  the header's return contract suggests. Measured on two consumer cards under a 610-series driver:
  both entry points return success and write the whole structure — registration state "not
  supported", a success status, clique zero and an all-zero cluster uuid. The header documents a
  not-supported *return* for a device without a fabric and this driver does not use it. So the state
  gate, not the return code, is what withholds the record on an ordinary card, and the not-supported
  branch is reachable without any fabric hardware. The other two non-final states are not: observing
  them needs a card that is registering.
- **No family gate.** The Ascend detector skips its fabric reads on older generations because every
  call behind them is V2-only. Here the driver's own refusal is the cheaper and more accurate test
  than a compute-capability threshold this detector would have to maintain.
- **NVIDIA publishes no shape, size, member count, rack or endpoints.** Those fields stay absent
  rather than being filled with a stand-in, so `fabric.members` does not appear on an NVIDIA node —
  which the label construction already spells as "a manufacturer that does not report a size".
- **The published object cannot separate the reasons a record is absent, and does not try.** A card
  with no fabric, one part-way through registering, one whose registration failed and one naming no
  cluster all record nothing, and `DeviceFabric` carries no registration state to tell them apart.
  Adding one would widen a published API to carry a diagnostic, so the difference is reported by a
  `V(3)` line from the detector instead. That line is the only place it exists, because the driver
  answers all of these reads successfully and nothing else in the pass has anything to report.
- **Withholding is whole-record here, not per-field.** The UB record publishes a kind and endpoints
  beside a domain it could not identify because those are independently useful; this manufacturer
  reports nothing else, so a record carrying only `kind: nvlink` would assert what every NVLink card
  already is.
- **Two files outside the detector change.** `api/worker/v1alpha1/devices.go` changes **only to
  repair statements this work makes false** — no field, tag, shape or semantic. `pkg/nodefeature/fabric.go`
  changes **behaviourally**: the clique enters the reduced value, which is argued above.

### Boundaries

- **Always:** gate on the registration state before publishing either identifier; keep the id
  comparable across workers; let only a read that happened replace what was recorded.
- **Ask first:** any change to the `fabric.domain` value grammar after the first release that
  publishes an `nvlink-` value — from then on it is a breaking change to written selectors.
- **Never:** invent a domain id for an unregistered accelerator; add a label key for the clique;
  introduce an external dependency to answer what NVML answers directly.

### Risks and Mitigations

| Risk | Mitigation |
|---|---|
| Not exercised on fabric hardware. No machine with an NVLink domain was reachable, so the publishing half of the gate has never run against a real registration. | Stated and split rather than hidden. The withholding half is measured: consumer cards answer with the not-supported state and the record is withheld. Only the publishing half needs fabric-capable hardware, and it is recorded unticked under Cluster readings below, which is this debt's only record. It does not gate this spec. |
| An NVML hiccup withdraws a node from a domain it has not left. | The last answer is carried forward, and only a read that succeeded replaces it — **for as long as the process lives**. The remembered answers are process-local, so a refused read on the first pass after a device-manager restart still withdraws the label, on this path and on the Ascend one it follows. Stated rather than fixed: that first pass cannot tell "nothing remembered yet" from "this accelerator has no domain", so protecting it would also protect a node that genuinely left one. |
| Two workers render the same cluster uuid differently and so publish two domains. | The encoding is pinned by a test rather than by convention. |
| A partitioned domain over-promises co-location across nodes. | The clique is in the value, and a node holding two cliques publishes no key. |
| The version fallback doubles the refused calls on fabric-less cards. | Two symbol-resolved calls per accelerator per detect pass, neither an error. A refusal of the newer call is reported at `V(3)` whether or not the fallback then answers, since that refusal is the only sign a path that used to work stopped working. |
| The repaired symbol probe cannot be exercised on the development platform, which has no NVML at all. | Measured instead against a shipped driver: it exports the name the repaired probe uses and no `_v2` spelling of it, while exporting the `_v2` names the file's sibling probes use — so the defect and the repair are both established on the artifact that decides them, not on the header alone. |

## Design Details

### Commands

```bash
go test ./pkg/devicemanager/detector/nvidia/... ./pkg/nodefeature/...
make generate   # the DeviceFabric comment is copied into generated code
make lint
make lint docs < /dev/null
```

### Project Structure

```text
pkg/devicemanager/detector/nvidia/
  fabric.go         # the read, the carry-forward, the gate and the rendering
  fabric_test.go    # the four states, the status, the zero uuid, the encoding, the carry-forward
  device.go         # one line wiring the record onto the per-accelerator topology
pkg/nodefeature/
  fabric.go         # the clique enters the reduced value
  fabric_test.go
binding/nvml/library_device.go        # one symbol probe, repaired; hand-written, not generated
api/worker/v1alpha1/devices.go        # comment only; no field, no tag, no shape
docs/architecture/network-topology.md # the manufacturer table and the label's value grammar
```

### Code Style

```go
// The registration state is the gate, and passing it is what makes the two identifiers mean
// anything: the cluster uuid and the clique id are filled in by the fabric manager when the
// accelerator finishes registering. [...]
if info.state != nvml.GPU_FABRIC_STATE_COMPLETED {
    return nil
}
```

Conventions this follows: the driver read and the rendering are separate functions so the gate is
unit-testable without hardware; the reason for a prohibition sits beside it in the comment; no spec
identifiers appear in Go comments; file names are snake_case.

### Implementation Plan

- [x] **T1 · The fabric read, the gate and the rendering**
      Blocked by: None
      Owns: `pkg/devicemanager/detector/nvidia/fabric.go`, `pkg/devicemanager/detector/nvidia/fabric_test.go`,
      `binding/nvml/library_device.go`
      Gate: review
      Acceptance: a registration that is complete, successful and names a non-zero cluster renders
      `{kind: nvlink, id: <32 hex>, cliqueId: <n>}`; every other answer renders nothing and reports
      the state it read at `V(3)`. A refused read reuses the last answer this accelerator gave. The
      newer entry point's symbol probe names the symbol it calls.
      Verify: `go test ./pkg/devicemanager/detector/nvidia/...`

- [x] **T2 · Wire the record onto the per-accelerator topology**
      Blocked by: T1
      Owns: `pkg/devicemanager/detector/nvidia/device.go`
      Acceptance: the detector carries the remembered registrations and their mutex; one assignment
      beside `ConstructTopology`; nothing else in the detect loop changes.
      Verify: `go build ./...`

- [x] **T3 · The clique enters the node label's value**
      Blocked by: None
      Owns: `pkg/nodefeature/fabric.go`, `pkg/nodefeature/fabric_test.go`
      Gate: review
      Acceptance: `<kind>-<id>-<clique>` where a clique is reported, `<kind>-<id>` where none is, no
      key at all when one node holds two cliques of one cluster, and the widest value a manufacturer
      can report is published rather than withheld. Ascend's published values are unchanged.
      Verify: `go test ./pkg/nodefeature/...`

- [x] **T4 · Repair the statements this work makes false, and regenerate**
      Blocked by: T1, T3
      Owns: `api/worker/v1alpha1/devices.go`, `api/worker/v1alpha1/generated.proto`,
      `api/worker/v1alpha1/zz_generated.crds.go`, `api/worker/zz_generated.openapi.go`,
      `pkg/kubeclients/applyconfiguration/worker/v1alpha1/devicefabric.go`
      Gate: review
      Acceptance: no sentence in the repository says one detector fills this record; the generated
      copies match the source; no field, tag, shape or semantic changed.
      Verify: `make generate` from a checkout whose path ends in the module import path, then
      `git status` names those four generated files and nothing else.

- [x] **T5 · The documentation page**
      Blocked by: T1, T3
      Owns: `docs/architecture/network-topology.md`
      Acceptance: the manufacturer table, the registration gate, the value grammar and the reading
      recipe all state what the code does.
      Verify: `make lint docs < /dev/null`

T1 and T3 carry no edge between them: T3's cases build `device.Fabric` values directly and never
reach a detector, and the two own disjoint packages. T4 and T5 depend on both for truth rather than
for order — either landing alone leaves the repository asserting something the code does not do.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to
make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

None. The reduction in `pkg/nodefeature/fabric_test.go` already carried cases over Ascend-shaped
records, and those are what establish that this change leaves the other manufacturers alone. They are
kept unchanged on purpose: a global rename of their two-part values would delete the control that
proves the three-part form did not reach a manufacturer reporting no clique.

#### Unit tests

- `pkg/devicemanager/detector/nvidia`: 2026-09-20 - 35.2% of statements (package). Both functions
  carrying this change's logic are at 100%: `newFabric` and `rememberFabric`. `readFabricInfo` and
  `readFabric` stay at 0% — they need an NVML device handle.
- `pkg/nodefeature`: 2026-09-20 - 92.3% of statements (package); `ConstructFabricNodeLabels` and
  `soleFabricDomain` both 100%.

What the cases pin: the four registration states plus one outside the vendor's enumeration, the
three non-publishing ones differing from a publishing baseline in exactly the state; a complete
registration whose status failed; an all-zero cluster uuid; clique zero, which is a clique rather
than an absence; a nil answer. Then the id-to-member wiring over distinct values, so a field taking
another's is visible; the id encoding's width, case and delimiters; and the carry-forward walked in
both directions — withdrawal through a non-final state, acquisition through a late registration, and
replacement of one complete domain by another, which is the recabling case. On the label side: the
clique composed into the value, two cliques of one cluster withholding the key, a manufacturer
reporting none keeping the two-part form, and the widest value a manufacturer can produce publishing
rather than being withheld.

Not covered, deliberately: `DetectAccelerator` end to end, which needs an NVML handle. The driver
read itself is also uncovered, and that exclusion is where this change's one shipped defect lived —
a symbol probe naming a function that does not exist. The layer takes a concrete `nvml.Device`, so
no fake can be injected; what caught the defect was reading the vendored header and then a driver's
exported symbols, which is why both are cluster readings rather than a test.

#### Integration tests

None, and structurally rather than as a deferral: this feature's only integration is a driver call,
and no fake NVML exists in this repository. That is why the gate is a pure function reachable
without a device handle.

#### e2e tests

None is written here. The cluster readings below are the outstanding ones, split as described, and
neither half gates this spec.

#### Cluster readings

None of these gates this spec, and **this section is their only record**: no standing collection
covers this subsystem's outstanding readings, so a pointer to one elsewhere would name a destination
that does not exist. Each is written to be executable without reading anything else — what to read,
what the answer must be, and what does not count as having taken it.

They split by what they need. The first group can be taken on any NVIDIA node and is the positive
baseline that makes the negative unit cases mean something; it is taken. The second needs a machine
shape nobody here has.

**On any NVIDIA node, no fabric hardware required — the not-supported state, end to end.**
The driver half of this is taken: two consumer cards under a 610-series driver answer both entry
points with success and a not-supported registration state, and the library exports
`nvmlDeviceGetGpuFabricInfo` and `nvmlDeviceGetGpuFabricInfoV` and no `_v2` spelling of either. What
remains is the same node carrying a build of this change.

**Read the next paragraph before taking any of these.** On a node with no fabric, the absences below
are what the *unchanged* code produces too: before this change the NVIDIA detector never touched the
field, so "no record" and "no label" hold byte for byte either way. Asserting them there is a check
that cannot fail, and it would read as a pass. The one signal that separates this change from its
absence on such a node is the `V(3)` line, which is why it leads.

- The detector reports the registration it read: a `V(3)` line naming the state as not-supported,
  once per accelerator per pass. *Does not count:* the two absences below, which the unchanged code
  produces as well; nor a pass that logged nothing, which is what an unbuilt or undeployed change
  looks like.
- The driver answers rather than refuses. *Does not count:* reading it off the host CLI. Both its
  full dump and `--query-gpu=fabric.clusterUuid,fabric.cliqueId` print `N/A` here — measured — and
  `N/A` is what the CLI prints for a query it refused and for one it answered with an unregistered
  accelerator alike. Nor does reading the header, which documents a return this driver does not use.
  The reading has to come from the API.
- This package's newer entry point answers at all — taken, and the recipe is worth keeping because
  the path had never executed anywhere. A throwaway env-gated test in `binding/nvml` calling
  `GetGpuFabricInfoV().V2()`, run in a container with the driver injected
  (`docker run --gpus all -e NVIDIA_DRIVER_CAPABILITIES=utility,compute` over a `golang` image with
  the tree mounted), returned success on both cards. That establishes the repaired symbol probe and
  the struct version the vendor accepts. *Does not count* as establishing the field decoding: on a
  card with no fabric every field is zero, so comparing the two struct versions field by field
  compares zeros with zeros and passes however the offsets are laid out. Confirming the decode needs
  a card whose fabric fields are not zero, which is the reading below.
- Given the line above, the two absences are then worth confirming as consistency rather than as
  evidence: `.spec.groups[].accelerators[].topology.fabric` absent on every accelerator, and neither
  `feature.gpustack.ai/fabric.domain` nor `fabric.members` on the node while other
  `feature.gpustack.ai/` keys are. *Does not count:* treating either as proof the change works;
  a present-but-empty fabric object or one with `kind: nvlink` and an empty `id`, which would be a
  record published without a domain; a node whose device manager never completed a pass; or absence
  of the whole label prefix, which says the label pass did not run rather than that it withheld.

**On a Hopper-or-newer machine with an NVLink fabric — the state gate. Unticked; the machine shape
is what it waits on, and nobody here has one.**

- The two remaining non-final states, "not started" and "in progress", are transient: they exist only
  while the fabric manager is registering an accelerator, so seeing one means sampling inside that
  window. **Not reproducible on demand.** It is recorded that way rather than as an item waiting to
  be ticked — whoever holds such a machine across a fabric manager restart can catch them, and nobody
  can schedule it. The not-supported state needs no window and is already measured.
- At `COMPLETED`: `topology.fabric` carries `kind: nvlink`, a 32-character lowercase-hex `id` and a
  `cliqueId`; the node carries `fabric.domain` equal to `nvlink-<id>-<clique>`. Cross-check the two
  identifiers against the host CLI, which names the same pair:
  `nvidia-smi --query-gpu=fabric.clusterUuid,fabric.cliqueId --format=csv,noheader`. *Does not
  count:* a label whose `id` differs from the `Devices` record, an `id` of 32 zeros, or a CLI
  reading of `N/A`, which is what an unregistered accelerator prints.
- Two nodes in one domain publish the same value. *Does not count:* one node's reading alone — the
  field exists to be compared across machines, and one machine cannot demonstrate that.
- The driver library exports the entry point the repaired probe names: `nvmlDeviceGetGpuFabricInfoV`
  is present in the shipped `libnvidia-ml`, alongside `nvmlDeviceGetGpuFabricInfo`. Taken, on a
  610-series driver. *Does not count:* reading the header instead of the shipped library, which is
  what established the defect and cannot establish the repair; nor observing that a fabric record was
  produced, since the fallback yields the same record from the older call and no signal distinguishes
  the two paths.
- The two struct versions decode to the same values on a card whose fabric fields are **not** zero.
  This is the half the fabric-less reading cannot reach, and it is what would catch a field offset
  that is wrong in this package rather than in the driver. *Does not count:* the same comparison on a
  card reporting all zeros, which passes for any layout.

## Alternatives

- **Read only the older struct version**, as the task brief specified. Rejected: the vendored header
  marks it for removal, and this detector's neighbouring reads use the newest-first fallback. What
  the rejection actually cost was not the six lines of fallback but the defect underneath them — the
  newer path had never run, and only a caller that wanted it found out. Taking this alternative
  would have left that in place for the next caller.
- **Gate the read on compute capability at or above Hopper.** Rejected: it duplicates in our code a
  fact the driver already answers, and it would be wrong the first time the vendor ships a fabric on a
  part we did not predict.
- **Publish the record and leave `id` empty until registration completes**, as the UB path does for an
  unidentified domain. Rejected: on Ascend the record still carries a shape and endpoints; here it
  would carry only `kind`, which asserts nothing.
- **Compare the clique beside the value instead of inside it.** Rejected: it fixes the node-internal
  case and leaves the cross-node one, which is the case the field's comment actually warns about.
- **Render the cluster uuid in dashed 8-4-4-4-12 form**, which is how the manufacturer's own library
  renders the same bytes. Rejected — but not because the bytes are not a UUID; the manufacturer's
  tooling treats them as one, and the opposite claim would be wrong. Rejected because this key is
  cross-manufacturer and the grammar of its value is part of its contract: with an undelimited id,
  `<kind>-<id>[-<clique>]` splits the same way for every manufacturer and the segment count is two or
  three, while a dashed id makes the count manufacturer-specific. The convenience it would buy — an
  operator visually matching our value against the manufacturer's own node label — is conditional on
  that other component being installed, which this design deliberately does not require, and it is
  recoverable at any time from the `Devices` record.

## Open Questions

1. **What should a health mask reporting degraded bandwidth or route recovery do?** The newer struct
   carries one and this detector ignores it. A domain that is registered but degraded is still one
   domain, so it does not belong in this gate — but it may belong in `Devices` as a health signal,
   which is a question about the unhealthy-accelerator surface rather than about this field.
2. **Does a node ever hold accelerators in two cliques in practice?** More than one clique per
   *cluster* is ordinary — the manufacturer's own documentation describes one rack being split into
   several, and a clique never spanning racks. More than one per *node* is not established, and the
   manufacturer's components treat it the way this one does: the device plugin withholds its label,
   the topology service calls such a node ambiguous and refuses it. So the guard is the industry's
   answer as well as ours, and what remains open is only whether it ever fires — which decides
   whether it is a real path or an unreachable one, and an unreachable guard is worth knowing about.
