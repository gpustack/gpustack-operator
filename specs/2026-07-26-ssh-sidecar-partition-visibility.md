# Spec: SSH Sidecar Partition Visibility — give the sidecar its workload's partition, not the parent card

Status: Shipped
Type: Bug fix

## Summary
When an SSH-enabled Instance is backed by a **hardware partition**, its workload container is confined to the
partition while its SSH sidecar is handed the **parent card's** identity. The sidecar's visibility path reuses
the physical *cards* the workload reserved and asks the vendor responder for those cards'
visible-devices env; the reservation records the partition's profile and the memory-slice intervals it
occupies, but not the partition's device identity, so the responder has nothing else it could answer with.
That violates an invariant this codebase already committed to in writing: the sidecar must receive a
device-cgroup permission for the **same physical device** the workload was allocated. This spec restores it.
Doing so requires the vendor responder to be askable for a *partition's* container response rather than a
*card's* — a contract change deliberately deferred out of the logical/physical slicing split into this spec.

## Motivation

The SSH sidecar exists so a user can log into their Instance and work with the accelerator it was given. For
every other accelerator family that promise holds: an exclusive Instance's sidecar gets the card, a
logically sliced Instance's sidecar gets the same slice artifacts as the workload. For the partition family it
does not.

### What the sidecar's allocation actually controls

The mechanism is not new, and it is not env visibility. It is stated and PoC-validated in
[the SSH-Instance slicing spec](./2026-07-04-ssh-instance-accelerator-slicing.md) (feature F2), and reading
this spec without it invites the wrong mental model:

- The sidecar's `sshd` runs `ForceCommand /chroot.sh`, which `nsenter`s into `main`'s mount/UTS/IPC/net/PID
  namespaces, `chroot`s into `main`'s rootfs, and imports `main`'s environment from `/proc/<pid>/environ`.
  So the **environment and filesystem an SSH user sees are `main`'s**, including its visible-devices variable.
- `nsenter` does **not** change cgroup membership. The session therefore stays in the **sidecar's** cgroup, so
  the **sidecar's own device-cgroup grant** is what decides which device nodes the SSH user may open. The
  origin spec records the PoC negative control: a sidecar without the grant leaves `main`'s devices
  unreachable from the SSH shell.

That is why the sidecar holds an accelerator allocation at all, and it is the sole thing that allocation buys.
`getResourceRequirements` says as much at the call site — the sidecar requests the visibility resource with
`main`'s card count, "giving the sidecar a narrow device-cgroup grant".

### Measured, on a real MIG-capable node

A partition-backed SSH workload holding a `3g.40gb` instance:

```
main env : NVIDIA_VISIBLE_DEVICES=MIG-<instance-uuid>     cuInit=0 (CUDA_SUCCESS)
sshd env : NVIDIA_VISIBLE_DEVICES=GPU-<parent-card-uuid>
```

The workload is confined to its partition. The sidecar's grant is the parent card. The in-container legs of
that observation — `nvidia-smi -L` and a trivial CUDA init inside the sidecar — came back empty, but they are
**inconclusive rather than reassuring**: `kubectl exec -c sshd` lands in the tooling-free Alpine rootfs, so an
absent probe says nothing about what was injected. The env is decisive on its own, being what the container
runtime consumes to build the device set and the cgroup rules.

### Re-measured after the fix, on the same class of node

The same workload, on a Kubernetes cluster with an 8-card MIG-capable NVIDIA node, running the operator image
built from this branch (`case-28.sh`, profile `3g.40gb`, workload image `python:3.12-slim`, card
`GPU-7b5a7507-…`):

```
main env : NVIDIA_VISIBLE_DEVICES=MIG-9f5d5c93-dc85-5287-952c-64b03644b0b0   cuInit=0 (CUDA_SUCCESS)
sshd env : NVIDIA_VISIBLE_DEVICES=MIG-9f5d5c93-dc85-5287-952c-64b03644b0b0
```

Identical, and no `GPU-` card identity anywhere in the sidecar's grant — `main`'s `nvidia-smi -L` names the
parent card and its one `3g.40gb` instance, while the sidecar is told about the instance alone. The sidecar's
in-container probe stays INCONCLUSIVE for the same reason as before (no `nvidia-smi` in that image), so the
guard asserts on the env, which is what the runtime consumes.

### Why it happens

- `allocateVisibility` (`pkg/deviceplugin/server.go`) does not select a device. It re-finds the pod, takes the
  accelerator reservation held by another container of the same pod, and reuses that reservation's cards as
  the "allocated" set for `Responder.GetContainerAllocateResponse`.
- That responder method's input is `map[Resource]int32` — a **card**-keyed map (`Resource{Group, Device}`,
  where `Device` is the card UUID for NVIDIA). A card is all it can be told about, so a card is all it can
  answer with.
- The partition's identity is produced by `PhysicalSlicedActuator.ActuatePhysicalSliced`, which returns a
  `PhysicalSlicedAllocation` carrying `Profile`, `Placements` and the container `Response` that names the
  partition. Only the profile and the placements are propagated onward — into the Pod's allocation annotation
  (`AllocatedPhysicalProfile`, `AllocatedPhysicalPlacements`) and the in-process reservation. The partition's
  device identity is consumed by the workload's own response and then dropped.
- The device IDs kubelet passes are no help: for both pooled modes the tokens are a flat interchangeable
  per-card pool (`Resource.GetDeviceIds`), so `…:0004` is an index, not a placement.

The one place the identity survives on the node is the NVIDIA MIG **ownership marker** written under
`OperatorPodsDir` by `reserveMigInstance`, which records `MigUUID` keyed by pod UID, container and card. It is
vendor-specific and durable, where the reservation is vendor-neutral and in-process only.

### What it violates

No measurement is needed to classify this. The origin spec's F2 states the contract — a device-cgroup
permission for the *same* physical device `main` holds — and a parent-card grant on a card in a partitioning
mode is broader than that by construction: the parent card hosts every partition carved on it, including other
tenants'. The severity therefore does not hinge on whether CUDA can be driven through the parent handle in MIG
mode; the grant is already wider than the Instance paid for. The fix is to restore the invariant, and the
release note says so plainly.

### Goals

- The SSH sidecar of a partition-backed Instance receives a **partition identity**, never the parent card and
  never a grant broader than one partition; and when several same-shaped Instances are admitted to the node
  together, a **distinct** partition per sidecar rather than all of them the oldest Pod's. Binding a sidecar to
  *its own* workload's partition rests on the same oldest-pending heuristic the workload path uses
  (`getAllocatingPod`), because the device-plugin `Allocate` RPC carries no Pod identity; under a concurrent,
  out-of-creation-order arrival two sidecars can still be swapped onto each other's partition — a bound this
  path shares with the workload path and does not widen (see Risks and Open Questions).
- The vendor responder can be asked, for a container that already holds a partition on a card, what that
  container's visible-devices response is — a contract that is explicit rather than inferred from a card map,
  and shaped so a vendor that injects **device nodes** instead of a `*_VISIBLE_DEVICES` env can answer it.
- The path fails **closed**: when the partition's identity cannot be resolved, or cannot be shown to still be
  live, the sidecar's admission is rejected rather than served a parent-card, stale or empty response.
- The partition's identity is resolved from a **durable, node-local record**, not from in-process state, and
  the sidecar's co-allocation itself survives a device-manager restart between the two `Allocate` calls.
- The existing observational e2e case becomes a regression guard by asserting the sidecar's env names the
  partition.

### Non-Goals

- Changing what the **workload** container receives; it is already correct.
- Changing the visibility resource, its token pool, or the fact that visibility consumes no ledger units and
  holds no reservation of its own. T8 records that a visibility `Allocate` was answered for a container, but
  that record names no device and costs no unit — it is an identification marker, not a reservation.
- Giving the sidecar a *different* partition from its workload, or more than one.
- Vendors with no hardware partitioning. Their visibility path is unchanged.
- The confined-shell and SSH-server behavior of the sidecar itself.
- The partition **reclaim window** — a same-profile replacement admitted inside it can be handed an instance
  the reclaimer is about to destroy. That is a pre-existing defect tracked by its own (still local) spec,
  `2026-07-26-partition-reclaim-window-toctou`; this spec only narrows its blast radius on the visibility
  path (see Risks).

## Proposal

Make the partition's identity reach the visibility path, and give the responder a contract that can express
it.

**Widen the existing optional capability rather than adding a second one.** `PhysicalSlicedActuator` is
renamed `PhysicalSlicedResponder` and gains a second method that answers the visibility question for a
partition-backed container: given the pod, the sidecar container being served, the cards the owner holds, and
the **name of the container that holds them**, return the container response naming the **partitions** on
those cards. One interface means one type assertion and makes "a responder that can carve a partition can
name it" a compile-time invariant instead of a runtime hole.

The method returns a full `*ContainerAllocateResponse`, not an env string. Vendors differ in how they make a
device visible: NVIDIA injects `NVIDIA_VISIBLE_DEVICES`, while T-Head injects device nodes directly
(`pkg/devicemanager/allocator/thead/deviceplugin.go` appends `/dev/alixpu_ppu<index>` to `Response.Devices` and
sets no env at all), and Hygon and others follow that shape. Substituting a partition identity for a card
identity is therefore a change *inside the vendor's own response shape*; the server never inspects it.

`allocateVisibility` calls the capability when the container that holds the accelerator **requests a partition
profile**, and falls back to today's card-based `GetContainerAllocateResponse` otherwise. The trigger is the
owner container's own resource request (`partitionProfileOf`), not the reservation's `AllocatedPhysicalProfile`
— the request is in the Pod spec from the start and is immune to the order in which the workload's `Allocate`
publishes and re-publishes its reservation. A responder that lacks the capability, or that cannot resolve the
identity, fails the admission closed exactly as a stale or mismatched reservation already does.

The NVIDIA implementation resolves the identity from the owning container's MIG ownership marker on that card
— the same file the reclaimer treats as the authority on who owns which instance — and then **verifies under
the card lock that the recorded `MigUUID` is still the live instance's**, mirroring the guard the retry path
already applies at `reserveMigInstance`. Without that verification the sidecar could be handed a destroyed
instance's UUID, or — after GPU-instance id reuse — another tenant's partition.

`allocateVisibility` additionally falls back to the Pod's durable allocation annotation when the in-process
reservation is absent, so a device-manager restart between the workload's `Allocate` and the sidecar's no
longer strands the sidecar.

### User Stories

#### Story 1
As a user of an SSH-enabled Instance backed by a MIG partition, I want the accelerator I can actually open
from my SSH shell to be the partition my Instance was given, so that the shell I log into can drive the same
accelerator my workload runs on — the shell inherits `main`'s environment and rootfs, but only the sidecar's
device-cgroup grant decides what it may open.

#### Story 2
As a user, I want to run a small CUDA program over SSH against my partition, so that SSH is usable for
debugging a partition-backed workload and not only for browsing the filesystem.

#### Story 3
As a cluster administrator sharing one card between tenants, I want a sidecar's accelerator grant to be no
broader than the partition its Instance holds, so that SSH access cannot become a wider handle than the
workload itself has — on a card that may host other tenants' partitions.

#### Story 4
As a maintainer, I want the visibility path to reject an admission it cannot resolve, so that no container is
ever started with a grant a runtime could read as "the whole card", and none is started against a partition
that no longer exists.

### Core Features & Acceptance Criteria

- **The sidecar's response names the partition.** For a partition-backed SSH Instance, the sidecar's
  visible-devices env equals the workload container's.
  *Acceptance:* the e2e SSH-sidecar-on-a-partition case asserts `sshd env == main env` and that the value is a
  partition identity, not a card identity; the case's existing `F11_EXPECT=own` mode becomes its default.
- **A responder contract that can express a partition, in any injection shape.** The widened capability is
  documented on the interface with what the server guarantees about when it is called, and returns a full
  container response so a device-node-injecting vendor can answer it. A responder that does not implement the
  capability keeps today's behavior for every non-partition family.
  *Acceptance:* unit tests cover a responder that implements it, one that does not, and one that returns an
  error; the non-implementing case is byte-identical to the current response.
- **The trigger is the owner's request, not the reservation's bookkeeping.** A partition-backed pod takes the
  capability branch even when the reservation the sidecar reads has not yet been re-published with its
  profile.
  *Acceptance:* a unit test with an owner container requesting a partition profile and a reservation carrying
  no `AllocatedPhysicalProfile` still takes the capability branch (and fails closed if it cannot be served).
- **Fail closed on an unresolvable or dead identity.** A missing, malformed or wrong-card ownership record —
  or one whose `MigUUID` no longer matches the live instance — rejects the visibility admission with
  `FailedPrecondition`, never a parent-card, stale or empty response.
  *Acceptance:* unit tests for each of those four inputs assert the error and assert no
  `ContainerAllocateResponse` is returned.
- **Survives a device-manager restart between the two Allocates.** Both the co-allocation and the identity are
  read from durable sources, not the in-process reservation.
  *Acceptance:* a unit test resolves and serves the sidecar with an empty in-process reservation map, the Pod
  annotation present and the ownership record present.

### Notes / Constraints / Caveats

- Accelerator claims live in exactly one container group, so a pod has exactly one accelerator-holding
  container for the sidecar to co-allocate from; the resolver may rely on that and must not silently pick one
  of several.
- The owner container is picked by one shared rule — exclude self, then take the lexicographically smallest
  remaining container name — and **both** the reservation lookup and the annotation fallback must use it. The
  cards and the owner name must come from the same pick, or the resolver would read one container's marker
  against another container's cards.
- The in-process reservation is not durable and is pruned when the pod goes away; the Pod annotation and the
  NVIDIA ownership marker are durable. Prefer the durable sources, and treat the reservation as the fast path.
- `pkg/deviceplugin` is vendor-neutral: the resolution of a partition identity belongs behind the responder
  seam in `pkg/devicemanager/allocator/nvidia`, never in the server.
- Two containers of one pod sharing a single MIG device UUID is the intended shape — it mirrors what the
  logical-slicing families already do for a sidecar.
- Marker files are read by an already-shipped device manager; extending what the visibility path reads from
  them is safe, changing their format is not.
- `pkg/deviceplugin` links a Go plugin, so a test in that package must not pull in the cgo NVML binding; keep
  the NVIDIA-side resolution and its tests in the allocator package, behind the existing driver seam.
- The e2e guard compares visible-devices **envs**, which is NVIDIA-shaped. A future partitioning vendor that
  injects device nodes instead needs its own assertion; there is no such vendor in the tree today.
- **Logical and physical slicing are mutually exclusive per _card_, not per _pool_.** Detectors report the two
  capabilities exclusively for one card (`pkg/device/population.go`), which is why the per-card
  `device.IsLogicallySliceable` folds in `!IsPartitioned`. A pool aggregates cards of both kinds — the
  architecture doc records a **mixed** node advertising both families at once — so the pool-level predicates
  on `InstanceTypeAcceleratorDetail` must stay independent. Folding them (or offering a single "sliceable"
  predicate over their disjunction) would either starve a mixed pool of logical slices or let an
  all-partitioned pool read as logically sliceable, which is the exact over-advertisement the
  logical/physical split removed.
- T2 is a **naming refactor unrelated to the sidecar fix**, carried in this spec at the maintainer's request.
  It is safe to carry because it shares no owned path with any other task and therefore blocks nothing: the
  visibility fix lives in `pkg/deviceplugin` and `pkg/devicemanager/allocator/nvidia`, T2 in the API type and
  the two `pkg/worker` consumers. It should be reviewed — and, if the branch is split, shipped — on its own
  terms.

### Boundaries

- **Always:** fail closed when the identity cannot be resolved or cannot be shown live; keep visibility
  consuming no ledger units and holding no reservation; keep vendor specifics behind the responder interface.
- **Ask first:** any change to the marker file format; giving the sidecar an accelerator identity that differs
  from the workload's; changing the visibility token pool's size or shape.
- **Never:** emit an empty visible-devices response; grant the sidecar a card identity on a card in a
  partitioning mode; make the vendor-neutral server aware of MIG.

### Risks and Mitigations

- **A marker records an instance that no longer exists, or whose GPU-instance id has been reused by another
  tenant's partition** → the resolver verifies the recorded `MigUUID` against the live instance under the
  card lock before answering, mirroring the guard `reserveMigInstance` already applies on the retry path; a
  mismatch fails closed. This is also what narrows the out-of-scope reclaim-window defect's blast radius on
  this path — without it, the fix would widen that defect from "the workload's retry fails closed" to "the
  sidecar is granted a destroyed partition with no check at all".
- **The workload's post-actuation rollback destroys the instance while the sidecar's Allocate is mid-flight**
  → the liveness check closes most of the window; the remainder is benign, because a rollback means the
  workload's own `Allocate` failed and the Pod will not start either way.
- **A restart between the two Allocates loses the in-process co-allocation** → fall back to the Pod's durable
  allocation annotation; cover it with a unit test that empties the in-process map.
- **The annotation fallback weakens the documented "reservation pruned ⇒ fail closed" meaning** — a pod whose
  reservation was pruned but whose annotation survives now gets visibility. Resolved in T3, ungated: the only
  prune sweep runs from `Reconcile` over an unfiltered per-node Pod list that deliberately keeps terminating
  pods, so a reservation outlives its pod until the Pod object leaves the informer — at which point
  `getAllocatingPod` fails first and the fallback is never reached. The three `releaseReservation` rollbacks
  all follow an Allocate that failed, so the workload container never starts and a sidecar grant in that
  window buys nothing (and, for a partition, the rollback destroys the instance, so T1's liveness check
  rejects it anyway).
- **A vendor responder without the new capability silently regresses** → the fallback is the current code
  path, asserted byte-identical by a unit test; only a partition-requesting owner takes the new branch.
- **Failing closed turns a today-degraded sidecar into a rejected Pod** → that is the intended trade: today's
  degraded mode is not a cosmetic flaw but the invariant violation itself, and it is invisible to the user.
  It does make an unresolvable identity a user-visible admission failure, so the error message must name the
  pod, the card and the owning container, and transient causes must be genuinely transient (kubelet retries
  container creation with backoff).
- **A wrong-pod grant** is pre-existing, and the reason first recorded here — that `getAllocatingPod`'s
  matching gates it — is **wrong for the visibility path**; saying so mattered enough that the correction is
  kept rather than quietly replaced. Nothing gated it. **Closed by T8.** The workload path is
  protected by `SkipReserved`: a container whose own `Allocate` already reserved its cards is set aside, so
  the next call in a concurrent batch cannot re-pick it. Visibility deliberately opts out of that — it must
  re-find its own pod — **and reserves nothing of its own**, so no marker distinguishes a visibility container
  already served from one still waiting. `classify` compares only the resource name and quantity, and the
  answer is `feasible[0]`, the oldest pending pod. Two SSH Instances admitted together on one node with the
  same shape therefore have interchangeable sidecars, and `CreationTimestamp` is second-granular under a
  non-stable sort, so which one wins is arbitrary.

  Every fail-closed gate downstream then agrees, because they all evaluate inside the *mistaken* pod's
  context: its cards are present, its counts match, its marker is live. This spec does change the failure's
  shape — before it, a misresolved sidecar was granted the parent card, which happened to cover its own
  partition when both Instances landed on the same card; after it, the sidecar is granted precisely another
  tenant's partition and cannot see its own. Narrower, but no longer accidentally right.

  It is in scope after all — Goal 1 says the sidecar receives *its workload's* partition, and being handed
  another workload's violates that same invariant on the concurrency axis. T8 **narrows** it with a
  granted-`(podUID, container)` set kept **separate** from `reservations`: reusing `reserveDevices` would
  feed a visibility container into `reservedModeForResource`, `reservedPhysicalOccupied` and `liveDeviceIDs`,
  corrupting the very per-card accounting those exist for. What T8 buys is that the two sidecars resolve to
  **distinct** Pods instead of both to the oldest — not that each provably resolves to **its own**: the
  `Allocate` RPC still carries no Pod identity, so the RPC→Pod binding stays the oldest-pending heuristic, and
  a concurrent arrival out of creation order can still swap the two sidecars onto each other's partition. That
  residual is exactly the bound the workload path already carries through the same `getAllocatingPod`; T8 does
  not widen it, and closing it for good would take a cross-path Pod-identity correlation (see Open Questions).
  The live two-Instance verification below passed because kubelet's real `Allocate` ordering matched creation
  order.
- **A device-manager restart between two pods' sidecar Allocates loses the grants**, so both retries can
  resolve to the oldest pending pod again → the same exposure the reservation map already has, and narrower:
  kubelet does not re-`Allocate` a container whose checkpoint it still holds, so only admissions in flight at
  the instant of the restart are affected. Not closed; a durable record would have to be invented for a
  container that, by design, owns nothing to record.

## Design Details

### Commands

Go build, unit tests and lint run on the **local development host** (macOS): the whole module — including the
vendor CGO detector packages — compiles and tests there, verified by a read-only smoke build of both target
packages. The NVIDIA side is exercised through the existing driver seam, so the change and its regression
tests need no GPU. Confirming it end to end needs a **Kubernetes cluster with a MIG-capable NVIDIA node**
and an SSH client; container images are built on a **remote amd64 builder over SSH**. A cluster this run
provisions itself (from `testing/infra/clusters`) is destroyed afterwards; a cluster the user brings is not —
only the run's own resources are torn down.

```bash
make test        # whole module, including the vendor CGO detectors
make lint        # whole-module golangci-lint; the first cold run needs several minutes
make build

GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race \
  ./pkg/deviceplugin/... ./pkg/devicemanager/allocator/nvidia/...
```

### Project Structure

```
pkg/deviceplugin/
  types.go       # ContainerAllocateResponder, PhysicalSlicedResponder, PhysicalSlicedAllocation
  server.go      # allocateVisibility — the call site that branches on a partition-requesting owner
  controller.go  # _Reservation, reservedAcceleratorDevices, AllocatedAcceleratorsOf — the two co-allocation sources
pkg/devicemanager/allocator/nvidia/
  mig.go            # the ownership marker (markerPath / parseMarker) that records the partition identity
  mig_visibility.go # NEW — the partition-identity resolver behind the responder seam
  deviceplugin.go   # where the NVIDIA responder is assembled and the MIG driver is shared with Visibility
pkg/devicemanager/allocator/thead/
  deviceplugin.go   # the device-node injection shape the new contract must also serve
api/worker/v1alpha1/devices.go   # AllocatedPhysicalProfile / AllocatedPhysicalPlacements — the durable record
```

### Code Style

The capability keeps the shape it already has — an optional interface a responder may implement, documented
with what the server guarantees at the call site — and grows one method rather than spawning a sibling
interface:

```go
// PhysicalSlicedResponder is an optional capability of a ContainerAllocateResponder that
// owns a hardware GPU partition end to end: materializing it for the container that
// requests it, and naming it again for a sidecar that co-allocates the same partition.
// A responder that does not implement it cannot serve partition requests at all.
PhysicalSlicedResponder interface {
	ActuatePhysicalSliced(...) (*PhysicalSlicedAllocation, error)
	GetPhysicalSlicedVisibilityResponse(...) (*ContainerAllocateResponse, error)
}
```

Conventions this change follows: the server type-asserts the capability rather than widening the base
interface; a fail-closed rejection is a `grpccodes.FailedPrecondition` naming the pod, the owning container
and the card, and is logged at `Error` once; every branch is covered by a table-driven test against the
package's existing fake responders (`stubResponder`, `physicalActuatorResponder`) rather than a new mock.

### Implementation Plan

The signature is pinned here so the two wave-one tasks can be written in parallel against it:

```go
// GetPhysicalSlicedVisibilityResponse returns the container response naming the partitions the
// owner container already holds on the given cards. The server invokes it for a visibility (SSH
// sidecar) Allocate whose accelerator-holding container requests a partition profile, passing the
// container being served, the cards the owner holds, and the owner's name. It must resolve from a
// durable, node-local record — not in-process state — and must return an error, never an empty or
// parent-card response, when the identity cannot be resolved or cannot be shown to still be live.
GetPhysicalSlicedVisibilityResponse(
	ctx context.Context,
	pod *core.Pod,
	ctr *core.Container,          // the sidecar being served
	devs *workercore.Devices,
	allocated map[Resource]int32, // the cards the owner container holds
	owner string,                 // the container that holds the accelerator
) (*ContainerAllocateResponse, error)
```

- [x] **T0 · One name for the capability, one rule for the owner**
      Blocked by: None
      Owns: `pkg/deviceplugin/types.go`, `pkg/deviceplugin/server.go`, `pkg/deviceplugin/controller.go`,
      `pkg/deviceplugin/server_test.go`, `pkg/deviceplugin/controller_test.go`
      Gate: review
      Rename `PhysicalSlicedActuator` → `PhysicalSlicedResponder` (four sites: `types.go` ×2, `server.go`,
      a `server_test.go` comment). Lift the owner pick — exclude self, then lexicographically smallest — out
      of `reservedAcceleratorDevices` into one named helper, and return the owner's name alongside the
      allocation. `allocateVisibility`'s fail-closed messages carry that name and the cards, so an operator
      can diagnose a rejection without reading device-manager logs.
      Acceptance: no behavior change other than message text; every existing test passes unmodified except
      for the renamed symbol and the new return value; the two fail-closed messages name the pod, the owning
      container and the cards.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/deviceplugin/...`

- [x] **T1 · NVIDIA names — and proves live — the partition a container already holds**
      Blocked by: None
      Owns: `pkg/devicemanager/allocator/nvidia/mig_visibility.go`,
      `pkg/devicemanager/allocator/nvidia/mig_visibility_test.go`,
      `pkg/devicemanager/allocator/nvidia/deviceplugin.go`,
      `pkg/devicemanager/allocator/nvidia/mig.go`
      Gate: review
      A `GetPhysicalSlicedVisibilityResponse` method on the NVIDIA `*server` that, for each allocated card in
      `devs` order, reads the owner's MIG ownership marker (`markerPath(pod.UID, owner, cardUUID)`), verifies
      under `lockCard` that the recorded `MigUUID` is still the live instance's for that GPU-instance id, and
      joins the verified UUIDs into `NVIDIA_VISIBLE_DEVICES`. `New` constructs the MIG driver once, only when
      the partitioned server is registered, and shares it with the Visibility server so no second
      `nvmlInit` is taken and a node without partitioning initializes nothing. `mig.go` is owned for one
      extraction: the "allocated cards in `devs` order" loop becomes `allocatedCards`, shared with
      `ActuatePhysicalSliced`. Two copies could drift, and identical ordering is exactly what makes the
      sidecar's env equal the workload's.
      Acceptance: table-driven over a temp `OperatorPodsDir` with the fake MIG driver — all markers present
      and live → the env is the MIG UUIDs in `devs` order; marker missing → error, nil response; marker
      malformed or incomplete → error; marker recording a different card → error; marker present but the live
      instance's UUID differs (id reuse) → error; a single-card and a two-card case both covered.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race -run 'PhysicalSlicedVisibility' ./pkg/devicemanager/allocator/nvidia/`

- [x] **T2 · Name the pool's two slicing capabilities for what they are**
      Blocked by: None
      Owns: `api/worker/v1alpha1/instance_type.go`, `api/worker/v1alpha1/instance_type_test.go`,
      `pkg/worker/webhooks/worker/instance.go`,
      `pkg/worker/webhooks/worker/instance_test.go`, `pkg/worker/controllers/worker/instance.go`,
      `pkg/worker/controllers/worker/instance_test.go`
      Gate: review
      A drive-by naming fix, carried here because it is this subsystem's vocabulary and touches no file
      the visibility fix touches (see Notes). `InstanceTypeAcceleratorDetail.IsSliceable()` is renamed
      `IsLogicallySliceable()` — its body and its three call sites already mean exactly that, and only the
      name still claims the general concept. A sibling `IsPhysicallySliceable()` (`SlicedDetail.Physical.Count
      > 0`) joins it, and `validatePartitionedAcceleratorRequest` uses it for the guard its logical mirror
      already has: today a partition request against an all-logical pool falls through to
      "does not offer this partition profile; offered: []", which reads as a mistyped profile. Neither
      predicate may fold in the other: at the **pool** level the two capabilities are independently true
      (a mixed node advertises both), unlike the per-card `device.IsLogicallySliceable`, which does exclude
      partitioned cards.
      A rejection states the missing capability and stops there — it does not suggest the other request
      shape (maintainer's call, mid-task). That retires `unservedLogicalSliceMessage`,
      `unservedPartitionMessage` and, with them, `isAllPartitionedPool`: its remaining user
      (`validateExclusiveAcceleratorRequest`) only restated a rejection the whole-card `OnceMaxRequest` cap
      already delivers, since that view counts free unpartitioned cards and an all-partitioned pool has none.
      Acceptance: no `IsSliceable` identifier remains; the three logical call sites read
      `IsLogicallySliceable()` and behave identically; a partition request against a pool with no partitioned
      card is rejected with a message naming only the missing capability, and one against a partitioned pool
      that lacks the profile keeps today's offered-set message; a whole-card request on an all-partitioned
      pool is still rejected, by the `OnceMaxRequest` cap; the doc comments state why the pool-level
      predicates do not mirror the per-card exclusivity.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/webhooks/worker/... ./pkg/worker/controllers/worker/...`

- [x] **T3 · The sidecar's co-allocation survives a device-manager restart**
      Blocked by: T0
      Owns: `pkg/deviceplugin/server.go`, `pkg/deviceplugin/controller.go`,
      `pkg/deviceplugin/server_test.go`, `pkg/deviceplugin/controller_test.go`
      Gate: review
      When no in-process reservation exists for the pod, `allocateVisibility` falls back to the Pod's durable
      allocation annotation (`AllocatedAcceleratorsOf`, already the fallback `priorPartitionAllocation` uses),
      applying T0's shared owner pick to it. An unreadable annotation fails closed rather than falling through
      to the legacy path. The existing present/count gates are unchanged and keep rejecting partial or stale
      records.
      Acceptance: with an empty reservation map and a pod carrying the annotation, the sidecar's visibility
      Allocate succeeds and hands the responder exactly the owner's cards; with neither source, or with a
      malformed annotation, it returns `FailedPrecondition`; a non-partition vendor's response is byte-identical
      to the pre-fallback one.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race -run 'Allocate_Visibility' ./pkg/deviceplugin/`

- [x] **T4 · The visibility path asks for a partition, not a card**
      Blocked by: T1, T3
      Owns: `pkg/deviceplugin/types.go`, `pkg/deviceplugin/server.go`, `pkg/deviceplugin/server_test.go`
      Gate: review
      Add the pinned method to `PhysicalSlicedResponder`, documenting the server's guarantees at the call
      site. `allocateVisibility` branches when the owner container requests a partition profile
      (`partitionProfileOf` over the owner's container spec): it asserts the capability and calls it with the
      owner's name; a responder without the capability, or one returning an error, fails the admission closed.
      Every other reservation keeps today's `GetContainerAllocateResponse`.
      Acceptance: table-driven over the package's existing stub responders — partition-requesting owner +
      capable responder → the partition response; + incapable responder → `FailedPrecondition` and no
      `ContainerAllocateResponse`; + capability error → same; partition-requesting owner whose reservation
      carries no `AllocatedPhysicalProfile` → still the capability branch; non-partition owner → byte-identical
      to today's response.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/deviceplugin/... ./pkg/devicemanager/allocator/nvidia/...` then `make lint`

- [x] **T5 · case-28 becomes the regression guard**
      Blocked by: T4
      Owns: `.claude/skills/gpustack-operator-e2e/cases/case-28.sh`,
      `.claude/skills/gpustack-operator-e2e/SKILL.md`
      `F11_EXPECT` defaults to `own`, so the verdict asserts the sidecar's env names exactly `main`'s partition
      and carries no card identity. The SKILL.md case row and the environment-variable note stop describing the
      case as observational.
      Acceptance: `bash -n` clean; the SKILL.md row and the `F11_EXPECT` note describe a guard, not an
      observation.
      Verify: `bash -n .claude/skills/gpustack-operator-e2e/cases/case-28.sh`; live in T7.

- [x] **T6 · Docs record the sidecar's grant and its new seam**
      Blocked by: T4
      Owns: `docs/architecture.md`, `docs/operation/nvidia-mig.md`
      `architecture.md`'s SSH-Instance paragraph states that a partition-backed sidecar is served the partition
      through the responder capability, and that the co-allocation has a durable fallback;
      `nvidia-mig.md` records that the ownership marker is the visibility path's authority too, and that the
      read is liveness-checked.
      Acceptance: no doc still says the sidecar is granted the same *card* for a partition-backed workload.
      Verify: `grep -n "visibility" docs/architecture.md docs/operation/nvidia-mig.md` reads consistently with
      the shipped behavior.

- [x] **T6b · The device layer stops naming the SSH sidecar**
      Blocked by: T6
      Owns: comments only, across `pkg/deviceplugin/**` and every `pkg/devicemanager/allocator/*/deviceplugin.go`
      Added mid-build, on the observation that T0–T4 had propagated "the SSH sidecar" into comments on generic
      device-layer code. The SSH sidecar serves Instances; the device layer serves whatever container holds a
      visibility claim, and its comments must say that. A sweep of every pre-existing site says "a visibility
      request" / "the owner container" instead. `api/worker/v1alpha1/devices.go` and `pkg/deviceplugin/helper.go`
      are in scope for the same reason.
      Acceptance: no comment on vendor-neutral or non-NVIDIA device-layer code names the SSH sidecar; the diff
      is comment-only.
      Verify: `git diff --stat` shows no `.go` statement changed; `make test` and `make lint` stay green.

- [x] **Checkpoint A — the whole module is green before any cluster time is spent**
      `make test` and `make lint` pass on the branch.

- [x] **T7 · The cluster window: guard and no-regression sweep**
      Blocked by: T5, T6
      Owns: `specs/2026-07-26-ssh-sidecar-partition-visibility.md`
      Gate: review
      Provision a MIG-capable NVIDIA cluster, build and push the operator image on the remote amd64 builder,
      deploy it pinned by digest (a same-tag rebuild is otherwise served from the kubelet's cache), then run
      `case-28.sh` and the partition block (`run-partition-block.sh`) as a no-regression sweep. Record the
      observed `main`/`sshd` readings in this spec. Destroy the cluster.
      Acceptance: `case-28.sh` passes with the sidecar's env naming exactly `main`'s partition; the partition
      block shows no regression against its last recorded run; the readings are in this spec; every resource
      the run created is torn down, and a cluster the run provisioned itself is destroyed.
      Verify: `MIG_NODE_SSH=<user@host> .claude/skills/gpustack-operator-e2e/cases/case-28.sh <ns>`

      Ran against a Kubernetes cluster with an 8-card MIG-capable NVIDIA node. The operator image was built on
      a remote amd64 builder from a tree whose hash matches this branch's HEAD, deployed pinned by digest, and
      the deployment's own revision guard confirmed the running binary is this branch. The user brought the
      cluster, so it was not destroyed — only the run's own resources were torn down.

      `case-28.sh` **PASS**, readings recorded above. The rest of the partition family, against the last
      recorded run on `main`:

      | Case | This branch | Last run on `main` |
      |---|---|---|
      | 24, 25, 27, 31, 32 | PASS | PASS |
      | 34 | SKIP (node runs `topologyManagerPolicy: none`) | SKIP |
      | 28 | **PASS** — the guard this spec exists for | PASS as an observation |
      | 29 | PASS on re-run | PASS |
      | 30 | PASS on re-run | PASS |
      | 26 | 7 of 8 checks | already failing before this branch |

      No case regressed. `case-29` and `case-30` failed only on their first attempt, and both failures are
      recorded in the case log as the cluster's own connectivity dropping: `kubectl` reached the API through a
      tunnel that intermittently timed out, surfacing both as the credential plugin failing to mint a token
      and as a bare transport timeout to the API endpoint. Both passed every check once re-run over a
      retry-wrapped credential path.

      `case-26` is the one case still short of green, and it reproduces on a clean network, so it is a real
      failure rather than an artifact. It was **already failing on `main` before this branch**, and it now
      fails later than it did — the token-count assertion that failed on `main` passes here. The residual
      check is `the freed room is usable again`: a fresh partition request placed immediately after a
      whole-card instance is released is admitted while that instance is still being reclaimed, and hits
      `UnexpectedAdmissionError`. That is the reclaim-window race `case-31` exists to observe, which the same
      run recorded converging on its second attempt. It is tracked separately and is untouched by this spec:
      no visibility path runs in it.

      The only workload-path change this branch makes is the `allocatedCards` extraction and the responder
      type's rename, and the five partition cases that carve instances all passed, which exercises both.

- [x] **T8 · Two sidecars admitted together get their own partitions**
      Blocked by: T4, T7
      Owns: `pkg/deviceplugin/controller.go`, `pkg/deviceplugin/server.go`, `pkg/deviceplugin/server_test.go`
      Gate: review
      Added after the end-of-build review, which found the wrong-pod grant this spec had recorded as
      out of scope. It is not: T4 sharpened the failure from "another card, which happened to include my
      partition" into "precisely another tenant's partition, and never my own", and Goal 1 covers it.
      A `visibilityGrants` set on the reconciler, keyed by `(podUID, container)` exactly as `reservations`
      is, records that a visibility `Allocate` was answered for a container. It stays **separate** from
      `reservations` — an entry there would count in the per-card mode, occupancy and device-ID accounting
      every reader of that map performs, for a container that holds no card. `_AllocationMatch` gains
      `SkipGranted`, the visibility counterpart of `SkipReserved`, and `getAllocatingPod`'s
      already-reserved bucket widens into an already-*claimed* one that both flags feed, so the
      lost-checkpoint replay works identically for either. `allocateVisibility` takes the node allocate
      mutex across identify→grant, the same section the workload `Allocate` runs under, and rolls the grant
      back on every path that then rejects the admission, so kubelet's retry of that container resolves
      back to it. The grant is pruned from the `Reconcile` live-pod sweep beside the reservations.
      Acceptance: two same-shaped pending Pods, each with its workload container reserved on a card of its
      own, resolve their sidecars to their own cards rather than both to the oldest Pod's; an all-granted
      candidate set still replays the oldest instead of erroring; a rejected visibility Allocate leaves no
      grant; grants are per container, revocable one at a time, and never outlive their pod.
      Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/deviceplugin/...` then `make lint`

      The two-Pod case was written against the pre-fix code first and reproduced the defect exactly — the
      second sidecar was granted `dev-0`, the *first* Pod's card. `make test` 0 failures, `make lint` 0
      issues.

      Verified live as well, on a Kubernetes cluster with a MIG-capable NVIDIA node, against an image built
      from this branch and deployed pinned by digest. Two SSH Instances of the same profile were applied in
      one `kubectl apply` so their Pods were admitted together, and both landed on the **same physical
      card** — the condition the defect needs:

      ```
      both 2/2 Running   a -> h100-80gb-hbm3:GPU-fcb261a8-4542-a28b-0731-f56cb6825a9c
                         b -> h100-80gb-hbm3:GPU-fcb261a8-4542-a28b-0731-f56cb6825a9c
      instance a         main=MIG-cc2a2490-ac6c-5243-b426-472c9c7600c4  sshd=MIG-cc2a2490-ac6c-5243-b426-472c9c7600c4
      instance b         main=MIG-e3917d3c-4b35-551e-bdee-10ceb2e971f3  sshd=MIG-e3917d3c-4b35-551e-bdee-10ceb2e971f3
      ```

      Two distinct partitions on one card, each sidecar naming its own and carrying no `GPU-` card identity:
      8 of 8 checks PASS. `case-28`, the single-Instance guard, was re-run afterwards and still PASSes, so
      the grant and the mutex this task adds to that same path cost it nothing.

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
- `physicalActuatorResponder` (`pkg/deviceplugin/server_test.go`) implements the capability being widened, so
  it must gain the new method in the same change that widens the interface (T4) or the package stops
  compiling. `echoActuatorResponder` in the same file needs it for the same reason: it is asserted to the
  widened capability at runtime by the partition-actuation tests, so without the method those stop reaching
  the actuation path at all — a silent skip rather than a build failure, which is the worse of the two.
- T2's rename reaches three test comments and one test name
  (`TestInstanceWebhook_ValidateCreate_WholeCardOnSliceable`); the assertions themselves are unaffected,
  since the predicate's meaning does not change.
- No other prerequisite: `server_test.go` already carries the four `TestResourceServer_Allocate_Visibility*`
  cases, the `stubResponder` / `recordingResponder` fakes and the `visibilityReservation` fixture the new
  cases extend; `mig_test.go` already builds markers under a temp `OperatorPodsDir` against `fakeMigDriver`.

#### Unit tests
- `pkg/deviceplugin`: `2026-07-27` - `65.2%`
- `pkg/devicemanager/allocator/nvidia`: `2026-07-26` - `80.0%`
- `pkg/worker/webhooks/worker`: `2026-07-27` - `78.9%` (T2 only)
- `pkg/worker/controllers/worker`: `2026-07-27` - `63.3%` (T2 only)

New cases, each pinning one behavior:

1. Partition-requesting owner + marker present and live → the response env is the marker's `MIG-<uuid>`, never
   `GPU-<card>`.
2. Partition-requesting owner + responder without the capability → `FailedPrecondition`, nil response (catches
   a silent drop into the legacy parent-card path).
3. Marker missing / malformed / incomplete / recording a different card → `FailedPrecondition` for each.
4. Marker present but the live instance's UUID differs (GPU-instance id reuse) → `FailedPrecondition` (catches
   granting another tenant's partition).
5. In-process reservation absent + annotation present + marker present → the same `MIG-<uuid>` is served
   (the restart path, end to end).
6. In-process reservation absent + annotation present + non-partition vendor → response byte-identical to the
   pre-fallback one, including T-Head's index-keyed device nodes read from current inventory.
7. Partition-requesting owner whose reservation carries no `AllocatedPhysicalProfile` → still takes the
   capability branch (pins the trigger to the owner's request, not the reservation's bookkeeping).
8. Annotation naming a card no longer in inventory → count mismatch → `FailedPrecondition`.
9. Two non-self reservation/annotation entries → the owner chosen is the lexicographically smallest **and**
   the marker read uses that same owner's name (pins the cards and the owner to one pick).
10. Multi-card owner → the partition UUIDs are joined in `devs` order, matching the workload's own response.

T2 adds two more, on the pool-level predicates:

11. A **mixed** detail — `Logical.Count > 0` **and** `Physical.Count > 0` — reads true for both
    `IsLogicallySliceable()` and `IsPhysicallySliceable()` (pins that neither predicate excludes the other,
    the trap the identically-named per-card function sets).
12. A partition request against a pool with no partitioned card is rejected by the capability guard with the
    message naming the missing capability, while one against a partitioned pool that does not offer the
    requested profile still gets the offered-set message (pins the guard to capability, not membership).

T8 adds three, on the identification of the visibility container itself:

13. Two same-shaped pending Pods, each with its workload container reserved on a card of its own → the two
    sidecars' `Allocate` calls are served their **own** Pod's card, not both the oldest Pod's. Written
    against the pre-fix code first, where it failed exactly as the defect describes.
14. Both sidecars already granted → the search still answers, replaying the oldest (a kubelet that lost its
    checkpoint must not face a permanently failed admission).
15. A rejected visibility `Allocate` leaves no grant behind; grants are per container, revocable one at a
    time, and pruned with their pod.

#### Integration tests
None. The repository has no tier between unit tests and e2e: the device-plugin gRPC surface is exercised by
the unit tests against a fake client and fake responders, and by the e2e cases against a real kubelet.

#### e2e tests
- `case-28.sh` — the SSH sidecar on a hardware partition, promoted from observation to guard: the Pod reaches
  2/2 Running on a partition, and the sidecar's visible-devices env names exactly `main`'s partition with no
  card identity.
- `run-partition-block.sh` — the partition case block, as a no-regression sweep over allocation, cross-mode
  exclusion, placement arithmetic and reclaim.
- Not covered by e2e, deliberately: the restart-between-Allocates path (T3), which needs a device-manager
  restart inside a sub-second admission window and is pinned by unit test 5 instead.
- Not covered by a **committed** case, and still a gap: T8's two-sidecars-at-once scenario. No case admits
  two same-shaped SSH Instances together, and `case-28` (one Instance) passes with or without the fix, so it
  cannot stand in. T8 was verified live by a scratch script built on `_partition-lib.sh` — two concurrent
  partition-backed SSH Instances on one node, asserting each sidecar's visible-devices env names its own
  workload's `MIG-<uuid>` and that the two workloads hold distinct partitions. Promoting that script to a
  case belongs with the partition family (the shape `case-29` already builds for two differing profiles,
  run with identical ones).

## Alternatives

- **A second, visibility-only optional interface** beside `PhysicalSlicedActuator`. Keeps each interface to one
  responsibility, and was this spec's original shape. Rejected: it permits a responder that actuates a
  partition but cannot name it — a runtime hole that then needs its own guard test — where one widened
  interface makes the pairing a compile-time invariant for the price of a rename.
- **Carry the partition identity in the in-process reservation.** The smallest change: `PhysicalSlicedAllocation`
  already knows the identity, and the reservation is written on the same path. Rejected as the primary design
  because the reservation is in-process and pruned, so a device-manager restart between the workload's and the
  sidecar's Allocate would lose it and fail an admission that could have succeeded. Worth keeping as a fast
  path over the durable read.
- **Record the identity in the Pod's allocation annotation** beside `AllocatedPhysicalProfile`. Durable and
  vendor-neutral, and it would let anything in the cluster see which instance a Pod holds. Rejected for now:
  it is an API addition on a transport field whose stated purpose is the placement ledger, and it publishes a
  hardware identifier cluster-wide to solve a node-local problem.
- **Give the sidecar no accelerator at all.** Honest and safe, and it makes SSH useless for the one thing a
  GPU user wants a shell for. Rejected against Story 1.
- **Widen `GetContainerAllocateResponse` to take partition identities** instead of adding a capability.
  Rejected: it changes the signature for every vendor responder, including those with no partition support.
- **Return the partition identity as a `map[Resource]string` and let the server assemble the response.**
  Rejected: it cannot express a vendor that injects device nodes rather than an env, which is the majority
  shape outside NVIDIA.
- **Measure whether today's parent-card grant is usable before fixing it.** Rejected: the invariant the origin
  spec states is violated either way, so the measurement could only have changed the release-note wording, and
  it would have cost a cluster window of its own.

## Open Questions

- Should the sidecar's partition identity be resolved at Allocate time, or should the sidecar instead be made
  to *re-derive* it from the marker at container start? The former keeps the device-plugin contract as the
  single source of truth; the latter would also cover a partition replaced under a restarting workload.
- Does any non-NVIDIA vendor in the tree plan hardware partitioning? T-Head and Hygon are the candidates, and
  both inject device nodes rather than a visible-devices env — the contract is shaped for that, but the
  signature should be reviewed against the first real implementation before it is fixed by a shipped one.
- Should the RPC→Pod binding be made exact rather than heuristic? The sidecar (and the workload) resolve their
  Pod through `getAllocatingPod`'s oldest-pending guess only because the device-plugin `Allocate` RPC carries
  no Pod identity; a concurrent out-of-creation-order arrival can still swap two same-shaped sidecars onto each
  other's partition (see Risks). The kubelet **PodResources** API *does* expose the container→device mapping
  after the fact, so a reconciliation against it would close the swap residual for the sidecar and the workload
  path at once. Out of scope here — it touches the whole allocate path and needs its own cluster verification —
  but worth its own spec.
- ~~Is Pod deletion the only path that prunes an in-process reservation?~~ Answered in T3: yes for the prune
  sweep, and the three rollback releases cannot leave a startable container behind. The fallback ships
  ungated (see Risks).
